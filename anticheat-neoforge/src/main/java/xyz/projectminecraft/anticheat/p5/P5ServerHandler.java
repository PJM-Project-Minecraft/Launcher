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
import java.util.concurrent.Executors;
import java.util.concurrent.ScheduledExecutorService;
import java.util.concurrent.TimeUnit;

/**
 * СЕРВЕРНАЯ сторона P5. На входе игрока: шлём challenge, ждём ответ (или таймаут),
 * валидируем через бэкенд, кикаем в enforce-режиме.
 *
 * enforce на СЕРВЕРЕ не хранится — им управляет бэкенд (ANTICHEAT_P5_ENFORCE): он вернёт
 * allow=false только в enforce, иначе allow=true (репорт-онли). Мод просто исполняет allow.
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
    // Игрок → выданный challenge (пока ждём ответ).
    private static final ConcurrentHashMap<String, String> PENDING = new ConcurrentHashMap<>();

    /** Вызывать на входе игрока (PlayerLoggedInEvent). No-op, если P5 не сконфигурен. */
    static void onPlayerJoin(ServerPlayer player) {
        if (!P5Config.active()) return;
        String name = player.getGameProfile().getName();
        String challenge = randomHex(16);
        PENDING.put(name, challenge);
        PacketDistributor.sendToPlayer(player, new P5Payloads.P5Challenge(challenge));
        // Таймаут: нет ответа за окно → верифицируем с пустым proof (бэкенд отвергнет).
        TIMER.schedule(() -> {
            String pending = PENDING.remove(name);
            if (pending != null && player.connection != null) {
                verifyAndAct(player, name, pending, "");
            }
        }, P5Config.RESPONSE_TIMEOUT_MS, TimeUnit.MILLISECONDS);
    }

    /** Пришёл ответ клиента с proof. */
    static void onResponse(P5Payloads.P5Response msg, IPayloadContext ctx) {
        if (!(ctx.player() instanceof ServerPlayer player)) return;
        String name = player.getGameProfile().getName();
        String challenge = PENDING.remove(name);
        if (challenge == null) return; // уже обработан таймаутом
        verifyAndAct(player, name, challenge, msg.proof());
    }

    private static void verifyAndAct(ServerPlayer player, String name, String challenge, String proof) {
        boolean allow = verifyWithBackend(name, challenge, proof);
        if (!allow) {
            // Кик строго на серверном треде.
            player.server.execute(() -> {
                if (player.connection != null) {
                    player.connection.disconnect(Component.literal("Anticheat: не пройдена проверка защиты."));
                }
            });
        }
    }

    /** POST /api/anticheat/p5/verify. Возвращает allow (по умолчанию true при сетевом сбое —
     *  fail-open, чтобы недоступность бэкенда не кикала игроков). */
    private static boolean verifyWithBackend(String name, String challenge, String proof) {
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
            if (resp.statusCode() / 100 != 2) return true; // fail-open
            JsonObject o = JsonParser.parseString(resp.body()).getAsJsonObject();
            return !o.has("allow") || o.get("allow").getAsBoolean();
        } catch (Exception e) {
            return true; // fail-open при недоступности бэкенда
        }
    }

    private static String randomHex(int n) {
        byte[] b = new byte[n];
        RNG.nextBytes(b);
        StringBuilder sb = new StringBuilder(n * 2);
        for (byte x : b) sb.append(Character.forDigit((x >> 4) & 0xf, 16)).append(Character.forDigit(x & 0xf, 16));
        return sb.toString();
    }

    private static String jsonStr(String s) {
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
