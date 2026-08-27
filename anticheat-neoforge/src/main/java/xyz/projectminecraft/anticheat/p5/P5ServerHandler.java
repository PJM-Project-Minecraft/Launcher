package xyz.projectminecraft.anticheat.p5;

import com.google.gson.JsonObject;
import com.google.gson.JsonParser;
import net.minecraft.network.chat.Component;
import net.minecraft.server.level.ServerPlayer;
import net.neoforged.neoforge.network.PacketDistributor;
import net.neoforged.neoforge.network.handling.IPayloadContext;

import java.net.URI;
import java.net.http.HttpClient;
import java.net.http.HttpRequest;
import java.net.http.HttpResponse;
import java.nio.charset.StandardCharsets;
import java.security.SecureRandom;
import java.time.Duration;
import java.util.concurrent.ConcurrentHashMap;
import java.util.concurrent.ExecutorService;
import java.util.concurrent.Executors;
import java.util.concurrent.ScheduledExecutorService;
import java.util.concurrent.TimeUnit;

/**
 * СЕРВЕРНАЯ сторона P5. На входе игрока: шлём challenge, ждём ответ (или таймаут),
 * валидируем через бэкенд, кикаем в enforce-режиме.
 *
 * ANTICHEAT_P5_ENFORCE должен совпадать на игровом сервере и backend. Локальный флаг
 * определяет, создаётся ли bounded lease; ответ backend дополнительно проверяется и
 * расхождение конфигурации логируется.
 */
final class P5ServerHandler {
    private P5ServerHandler() {}

    private static final SecureRandom RNG = new SecureRandom();
    private static final HttpClient HTTP = HttpClient.newBuilder()
            .connectTimeout(Duration.ofMillis(P5Config.HTTP_TIMEOUT_MS)).build();
    private static final ScheduledExecutorService TIMER = Executors.newSingleThreadScheduledExecutor(r -> {
        Thread t = new Thread(r, "p5-timeout");
        t.setDaemon(true);
        return t;
    });
    // HTTP никогда не блокирует TIMER: иначе очередь 5-секундных timeout задерживает
    // challenge deadlines и, главное, проверку истечения connection lease.
    private static final ExecutorService HTTP_WORKERS = Executors.newVirtualThreadPerTaskExecutor();
    // Ник → конкретное подключение и его challenge. ServerPlayer входит в запись,
    // чтобы таймер старого подключения не забрал challenge нового входа с тем же ником.
    private record PendingChallenge(ServerPlayer player, String challenge) {}
    private static final ConcurrentHashMap<String, PendingChallenge> PENDING = new ConcurrentHashMap<>();
    private static final long VERIFY_RETRY_MS = 30_000L;

    /** Общий HTTP-клиент и планировщик — переиспользует поллер отзывов (P5RevokePoller). */
    static HttpClient http() {
        return HTTP;
    }

    static ScheduledExecutorService timer() {
        return TIMER;
    }

    static void executeHttp(Runnable task) {
        HTTP_WORKERS.submit(task);
    }

    /** Вызывать на входе игрока (PlayerLoggedInEvent). No-op, если P5 не сконфигурен. */
    static void onPlayerJoin(ServerPlayer player) {
        if (!P5Config.active()) return;
        if (P5Config.ENFORCE) P5RevokePoller.begin(player);
        String name = player.getGameProfile().getName();
        String challenge = randomHex(16);
        PendingChallenge pending = new PendingChallenge(player, challenge);
        PENDING.put(name, pending);
        PacketDistributor.sendToPlayer(player, new P5Payloads.P5Challenge(challenge));
        scheduleCheck(name, pending, 1);
    }

    /** Нет ответа за окно — перевысылаем challenge (клиент мог быть занят загрузкой мира
     *  на слабом канале). Кончились попытки → верифицируем с пустым proof (бэкенд отвергнет). */
    private static void scheduleCheck(String name, PendingChallenge expected, int attempt) {
        TIMER.schedule(() -> {
            if (PENDING.get(name) != expected || expected.player().connection == null) {
                return; // клиент уже ответил, вышел или ник принадлежит новому подключению
            }
            if (attempt < P5Config.CHALLENGE_ATTEMPTS) {
                // Отправка — строго на серверном треде (мы в таймере).
                expected.player().server.execute(() -> {
                    if (PENDING.get(name) == expected && expected.player().connection != null) {
                        PacketDistributor.sendToPlayer(expected.player(),
                                new P5Payloads.P5Challenge(expected.challenge()));
                    }
                });
                scheduleCheck(name, expected, attempt + 1);
                return;
            }
            if (PENDING.remove(name, expected)) {
                verifyAsync(expected.player(), name, expected.challenge(), "");
            }
        }, P5Config.RESPONSE_TIMEOUT_MS, TimeUnit.MILLISECONDS);
    }

    /** Пришёл ответ клиента с proof. */
    static void onResponse(P5Payloads.P5Response msg, IPayloadContext ctx) {
        if (!(ctx.player() instanceof ServerPlayer player)) return;
        String name = player.getGameProfile().getName();
        PendingChallenge pending = PENDING.get(name);
        if (pending == null || pending.player() != player || !PENDING.remove(name, pending)) {
            return; // уже обработан таймаутом либо это ответ старого подключения
        }
        verifyAsync(player, name, pending.challenge(), msg.proof());
    }

    private static void verifyAsync(ServerPlayer player, String name, String challenge, String proof) {
        executeHttp(() -> {
            VerifyResult result = verifyWithBackend(name, challenge, proof);
            player.server.execute(() -> applyVerifyResult(player, name, challenge, proof, result));
        });
    }

    private static void applyVerifyResult(ServerPlayer player, String name, String challenge,
                                          String proof, VerifyResult result) {
        if (!P5Config.ENFORCE) {
            if (result.reachable() && result.backendEnforce()) {
                P5ModServer.LOG.error("[P5] конфигурация расходится: backend enforce=true, игровой сервер=false");
            }
            return;
        }
        if (!P5RevokePoller.isCurrent(player)) return; // replacement уже занял UUID
        if (!result.reachable()) {
            scheduleVerifyRetry(player, name, challenge, proof);
            return;
        }
        if (!result.backendEnforce()) {
            P5ModServer.LOG.error("[P5] конфигурация расходится: backend enforce=false, игровой сервер=true");
        }
        if (result.allow() && P5RevokePoller.activate(
                player, result.nonce(), result.leaseRemainingMillis())) {
            return;
        }
        player.connection.disconnect(Component.literal("Anticheat: не пройдена проверка защиты."));
    }

    private static void scheduleVerifyRetry(ServerPlayer player, String name, String challenge, String proof) {
        TIMER.schedule(() -> {
            if (!P5RevokePoller.isCurrent(player)) return;
            verifyAsync(player, name, challenge, proof);
        }, VERIFY_RETRY_MS, TimeUnit.MILLISECONDS);
    }

    /** POST /api/anticheat/p5/verify. A network failure is distinguishable from a
     * report-only allow, so enforce mode can retry within the existing local lease. */
    private record VerifyResult(boolean reachable, boolean allow, String nonce,
                                long leaseRemainingMillis, boolean backendEnforce) {}

    private static VerifyResult verifyWithBackend(String name, String challenge, String proof) {
        String body = "{\"playerName\":" + jsonStr(name)
                + ",\"challenge\":" + jsonStr(challenge)
                + ",\"proof\":" + jsonStr(proof) + "}";
        try {
            HttpRequest req = HttpRequest.newBuilder(URI.create(P5Config.API + "/api/anticheat/p5/verify"))
                    .timeout(Duration.ofMillis(P5Config.HTTP_TIMEOUT_MS))
                    .header("Content-Type", "application/json")
                    .header("X-AC-P5-Secret", P5Config.SECRET)
                    .POST(HttpRequest.BodyPublishers.ofString(body, StandardCharsets.UTF_8))
                    .build();
            HttpResponse<String> resp = HTTP.send(req, HttpResponse.BodyHandlers.ofString());
            if (resp.statusCode() / 100 != 2) {
                P5ModServer.LOG.warn("[P5] бэкенд ответил {} на /p5/verify для {} — проверь ANTICHEAT_P5_SECRET",
                        resp.statusCode(), name);
                return new VerifyResult(false, true, "", 0L, false);
            }
            JsonObject o = JsonParser.parseString(resp.body()).getAsJsonObject();
            boolean allow = !o.has("allow") || o.get("allow").getAsBoolean();
            String reason = o.has("reason") ? o.get("reason").getAsString() : "";
            if (!reason.isEmpty()) {
                // Есть reason → proof не сошёлся. reportOnly:true — пускаем, но это сигнал обкатки.
                P5ModServer.LOG.info("[P5] хэндшейк {}: allow={} reason={} (reportOnly={})",
                        name, allow, reason, o.has("reportOnly"));
            }
            String nonce = o.has("nonce") ? o.get("nonce").getAsString() : "";
            long remainingMillis = o.has("leaseRemainingMillis")
                    ? o.get("leaseRemainingMillis").getAsLong() : 0L;
            boolean backendEnforce = o.has("enforce") && o.get("enforce").getAsBoolean();
            return new VerifyResult(true, allow, nonce, remainingMillis, backendEnforce);
        } catch (Exception e) {
            return new VerifyResult(false, true, "", 0L, false);
        }
    }

    private static String randomHex(int n) {
        byte[] b = new byte[n];
        RNG.nextBytes(b);
        StringBuilder sb = new StringBuilder(n * 2);
        for (byte x : b) sb.append(Character.forDigit((x >> 4) & 0xf, 16)).append(Character.forDigit(x & 0xf, 16));
        return sb.toString();
    }

    static String jsonStr(String s) {
        StringBuilder sb = new StringBuilder("\"");
        for (int i = 0; i < s.length(); i++) {
            char c = s.charAt(i);
            switch (c) {
                case '"' -> sb.append("\\\"");
                case '\\' -> sb.append("\\\\");
                case '\n' -> sb.append("\\n");
                case '\r' -> sb.append("\\r");
                case '\t' -> sb.append("\\t");
                default -> {
                    if (c < 0x20) sb.append(String.format("\\u%04x", (int) c));
                    else sb.append(c);
                }
            }
        }
        return sb.append('"').toString();
    }
}
