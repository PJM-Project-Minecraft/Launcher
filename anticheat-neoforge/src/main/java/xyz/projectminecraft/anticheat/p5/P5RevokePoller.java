package xyz.projectminecraft.anticheat.p5;

import com.google.gson.JsonArray;
import com.google.gson.JsonElement;
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
import java.util.ArrayList;
import java.util.List;
import java.util.concurrent.TimeUnit;

/**
 * Периодический опрос отзывов доступа. Закрывает дыру, из-за которой античит мог только
 * пожаловаться: и heartbeat, и самокик исполняет агент ВНУТРИ JVM игрока — прибил агента,
 * и всё, что происходило, это алерт оператору. Теперь решение принимает бэкенд, а кик
 * исполняет игровой сервер, до которого читер не дотягивается.
 *
 * Раз в POLL_INTERVAL_MS сервер шлёт бэкенду список онлайн-ников и получает, кого кикнуть.
 * Fail-open: недоступен бэкенд / не 2xx / кривой JSON — не кикаем никого (падение бэкенда
 * не должно выносить весь сервер). Enforce-режим держит бэкенд (ANTICHEAT_P5_ENFORCE):
 * в репорт-онли он отдаёт пустой список и только пишет в лог.
 */
final class P5RevokePoller {
    private P5RevokePoller() {}

    /** Интервал опроса. Кик отстаёт максимум на один интервал — этого достаточно:
     *  окно молчания агента на бэкенде и так 90с. */
    private static final long POLL_INTERVAL_MS = 25_000L;

    private static volatile boolean started = false;

    /** Запускается при старте сервера. No-op, если P5 не сконфигурен (нет секрета). */
    static void start(MinecraftServer server) {
        if (!P5Config.active() || started) return;
        started = true;
        P5ServerHandler.timer().scheduleWithFixedDelay(
                () -> pollOnce(server), POLL_INTERVAL_MS, POLL_INTERVAL_MS, TimeUnit.MILLISECONDS);
    }

    static void stop() {
        started = false;
    }

    private static void pollOnce(MinecraftServer server) {
        try {
            List<ServerPlayer> online = new ArrayList<>(server.getPlayerList().getPlayers());
            if (online.isEmpty()) return;
            StringBuilder names = new StringBuilder();
            for (ServerPlayer p : online) {
                if (names.length() > 0) names.append(',');
                names.append(P5ServerHandler.jsonStr(p.getGameProfile().getName()));
            }
            JsonArray kick = fetchKickList("{\"players\":[" + names + "]}");
            if (kick == null || kick.isEmpty()) return;

            for (JsonElement el : kick) {
                JsonObject o = el.getAsJsonObject();
                String player = o.has("player") ? o.get("player").getAsString() : "";
                String reason = o.has("reason") ? o.get("reason").getAsString() : "проверка защиты";
                if (player.isEmpty()) continue;
                P5ModServer.LOG.warn("[P5] кик по отзыву доступа: {} — {}", player, reason);
                // Кик строго на серверном треде (мы в планировщике).
                server.execute(() -> {
                    ServerPlayer target = server.getPlayerList().getPlayerByName(player);
                    if (target != null && target.connection != null) {
                        target.connection.disconnect(Component.literal("Anticheat: " + reason));
                    }
                });
            }
        } catch (Exception e) {
            // fail-open: любая ошибка опроса не должна ронять серверный тик.
        }
    }

    // Однократные лог-сигналы обкатки: «связь есть» и «связь пропала» пишем по одному разу
    // на смену состояния, а не каждые 25с (иначе консоль зашумится).
    private static volatile boolean linkOk = false;

    /** POST /api/anticheat/p5/revoked. null — не кикать никого (сбой/недоступность). */
    private static JsonArray fetchKickList(String body) {
        try {
            HttpRequest req = HttpRequest.newBuilder(URI.create(P5Config.API + "/api/anticheat/p5/revoked"))
                    .timeout(Duration.ofMillis(P5Config.HTTP_TIMEOUT_MS))
                    .header("Content-Type", "application/json")
                    .header("X-AC-P5-Secret", P5Config.SECRET)
                    .POST(HttpRequest.BodyPublishers.ofString(body, StandardCharsets.UTF_8))
                    .build();
            HttpResponse<String> resp = P5ServerHandler.http().send(req, HttpResponse.BodyHandlers.ofString());
            if (resp.statusCode() / 100 != 2) {
                // 401 = неверный/пустой секрет (самая частая тихая поломка обкатки).
                P5ModServer.LOG.warn("[P5] бэкенд ответил {} на /p5/revoked — проверь ANTICHEAT_P5_SECRET (совпадает с прод .env?)",
                        resp.statusCode());
                linkOk = false;
                return null;
            }
            if (!linkOk) {
                P5ModServer.LOG.info("[P5] связь с бэкендом OK (/p5/revoked отвечает 200)");
                linkOk = true;
            }
            JsonObject o = JsonParser.parseString(resp.body()).getAsJsonObject();
            return o.has("kick") && o.get("kick").isJsonArray() ? o.getAsJsonArray("kick") : null;
        } catch (Exception e) {
            if (linkOk) {
                P5ModServer.LOG.warn("[P5] опрос /p5/revoked не удался: {}", e.toString());
                linkOk = false;
            }
            return null;
        }
    }
}
