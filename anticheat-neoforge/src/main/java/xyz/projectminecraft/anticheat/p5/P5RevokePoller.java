package xyz.projectminecraft.anticheat.p5;

import com.google.gson.JsonArray;
import com.google.gson.JsonObject;
import com.google.gson.JsonParser;
import net.minecraft.network.chat.Component;
import net.minecraft.server.MinecraftServer;
import net.minecraft.server.level.ServerPlayer;

import java.net.URI;
import java.net.http.HttpRequest;
import java.net.http.HttpResponse;
import java.nio.charset.StandardCharsets;
import java.time.Duration;
import java.util.List;
import java.util.concurrent.ScheduledFuture;
import java.util.concurrent.TimeUnit;

/**
 * Серверно-авторитетные connection leases. Каждые 30 секунд backend подтверждает
 * конкретную пару UUID/login/nonce. Ошибка сети не кикает мгновенно, но и не продлевает
 * deadline: после 180 секунд без валидного renewal точное подключение отключается.
 */
final class P5RevokePoller {
    private P5RevokePoller() {}

    /** Интервал опроса. Кик отстаёт максимум на один интервал — этого достаточно:
     *  окно молчания агента на бэкенде и так 90с. */
    private static final long POLL_INTERVAL_MS = 30_000L;
    private static final long OUTAGE_GRACE_MS = 180_000L;
    private static final P5LeaseTracker<ServerPlayer> LEASES = new P5LeaseTracker<>(OUTAGE_GRACE_MS);

    private static volatile boolean started = false;
    private static volatile ScheduledFuture<?> pollTask;

    /** Запускается при старте сервера. No-op, если P5 не сконфигурен (нет секрета). */
    static void start(MinecraftServer server) {
        if (!P5Config.active() || !P5Config.ENFORCE || started) return;
        started = true;
        pollTask = P5ServerHandler.timer().scheduleWithFixedDelay(
                () -> P5ServerHandler.executeHttp(() -> pollOnce(server)),
                POLL_INTERVAL_MS, POLL_INTERVAL_MS, TimeUnit.MILLISECONDS);
    }

    static void stop() {
        started = false;
        ScheduledFuture<?> task = pollTask;
        pollTask = null;
        if (task != null) task.cancel(false);
        LEASES.clear();
    }

    static void begin(ServerPlayer player) {
        LEASES.begin(player.getUUID().toString(), player, System.currentTimeMillis());
    }

    static boolean activate(ServerPlayer player, String nonce, long remainingMillis) {
        return LEASES.activate(player.getUUID().toString(), player, nonce, remainingMillis,
                System.currentTimeMillis());
    }

    static boolean isCurrent(ServerPlayer player) {
        P5LeaseTracker.Lease<ServerPlayer> current = LEASES.current(player.getUUID().toString());
        return current != null && current.connection() == player;
    }

    static void remove(ServerPlayer player) {
        LEASES.remove(player.getUUID().toString(), player);
    }

    private static void pollOnce(MinecraftServer server) {
        if (!started) return;
        try {
            long now = System.currentTimeMillis();
            expireDue(server, now);
            List<P5LeaseTracker.Lease<ServerPlayer>> active = LEASES.snapshot();
            if (!active.isEmpty()) {
                StringBuilder connections = new StringBuilder();
                for (P5LeaseTracker.Lease<ServerPlayer> lease : active) {
                    ServerPlayer player = lease.connection();
                    if (connections.length() > 0) connections.append(',');
                    connections.append("{\"playerUuid\":").append(P5ServerHandler.jsonStr(player.getUUID().toString()))
                            .append(",\"playerName\":").append(P5ServerHandler.jsonStr(player.getGameProfile().getName()))
                            .append(",\"nonce\":").append(P5ServerHandler.jsonStr(lease.nonce())).append('}');
                }
                JsonArray renewed = fetchLeases("{\"connections\":[" + connections + "]}");
                if (renewed != null) {
                    for (var element : renewed) {
                        JsonObject result = element.getAsJsonObject();
                        if (!result.has("valid") || !result.get("valid").getAsBoolean()) continue;
                        String nonce = result.has("nonce") ? result.get("nonce").getAsString() : "";
                        long remainingMillis = result.has("leaseRemainingMillis")
                                ? result.get("leaseRemainingMillis").getAsLong() : 0L;
                        for (P5LeaseTracker.Lease<ServerPlayer> lease : active) {
                            if (lease.nonce().equals(nonce)) {
                                LEASES.renew(lease.key(), lease.connection(), nonce, remainingMillis, now);
                            }
                        }
                    }
                }
            }
        } catch (Exception e) {
            // fail-open: любая ошибка опроса не должна ронять серверный тик.
        }
    }

    private static void expireDue(MinecraftServer server, long now) {
        for (P5LeaseTracker.Lease<ServerPlayer> expired : LEASES.expired(now)) {
            P5ModServer.LOG.warn("[P5] lease истёк: {} nonce={}",
                    expired.connection().getGameProfile().getName(), expired.nonce());
            server.execute(() -> {
                ServerPlayer target = expired.connection();
                if (target.connection != null) {
                    target.connection.disconnect(Component.literal("Anticheat: соединение с проверкой защиты потеряно."));
                }
            });
        }
    }

    // Однократные лог-сигналы обкатки: «связь есть» и «связь пропала» пишем по одному разу
    // на смену состояния, а не каждые 25с (иначе консоль зашумится).
    private static volatile boolean linkOk = false;

    /** POST /api/anticheat/p5/lease. null means no renewal; local grace keeps play bounded. */
    private static JsonArray fetchLeases(String body) {
        try {
            HttpRequest req = HttpRequest.newBuilder(URI.create(P5Config.API + "/api/anticheat/p5/lease"))
                    .timeout(Duration.ofMillis(P5Config.HTTP_TIMEOUT_MS))
                    .header("Content-Type", "application/json")
                    .header("X-AC-P5-Secret", P5Config.SECRET)
                    .POST(HttpRequest.BodyPublishers.ofString(body, StandardCharsets.UTF_8))
                    .build();
            HttpResponse<String> resp = P5ServerHandler.http().send(req, HttpResponse.BodyHandlers.ofString());
            if (resp.statusCode() / 100 != 2) {
                // 401 = неверный/пустой секрет (самая частая тихая поломка обкатки).
                P5ModServer.LOG.warn("[P5] бэкенд ответил {} на /p5/lease — проверь ANTICHEAT_P5_SECRET",
                        resp.statusCode());
                linkOk = false;
                return null;
            }
            if (!linkOk) {
                P5ModServer.LOG.info("[P5] связь с бэкендом OK (/p5/lease отвечает 200)");
                linkOk = true;
            }
            JsonObject o = JsonParser.parseString(resp.body()).getAsJsonObject();
            return o.has("leases") && o.get("leases").isJsonArray() ? o.getAsJsonArray("leases") : null;
        } catch (Exception e) {
            if (linkOk) {
                P5ModServer.LOG.warn("[P5] опрос /p5/lease не удался: {}", e.toString());
                linkOk = false;
            }
            return null;
        }
    }
}
