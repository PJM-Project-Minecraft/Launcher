package xyz.projectminecraft.anticheat.p5;

import net.neoforged.bus.api.IEventBus;
import net.neoforged.fml.common.Mod;
import net.neoforged.neoforge.network.event.RegisterPayloadHandlersEvent;
import net.neoforged.neoforge.network.registration.PayloadRegistrar;

/**
 * КЛИЕНТСКАЯ точка входа мода P5 (jar с classifier=client, в профиль игроков). Содержит
 * ТОЛЬКО ответ на challenge: получил → HMAC(challenge, accessToken) → отправил. Серверной
 * логики (секрет, URL-ы /p5/verify и /p5/revoked, верификация, опрос отзывов) в раздаваемом
 * игрокам jar НЕТ — читеру нечего оттуда вытащить сверх честного потолка (см. README).
 *
 * modId "pjmac" общий с серверным jar → namespace канала совпадает.
 */
@Mod(P5Payloads.MOD_ID)
public final class P5ModClient {
    public P5ModClient(IEventBus modBus) {
        modBus.addListener(P5ModClient::registerPayloads);
    }

    private static void registerPayloads(RegisterPayloadHandlersEvent event) {
        PayloadRegistrar registrar = event.registrar("1");
        // Клиент ПРИНИМАЕТ challenge.
        registrar.playToClient(P5Payloads.P5Challenge.TYPE, P5Payloads.P5Challenge.CODEC,
                P5ClientHandler::onChallenge);
        // Клиент ОТПРАВЛЯЕТ ответ — тип надо зарегистрировать, но клиент его не принимает
        // (noop-хендлер, на клиенте не вызывается).
        registrar.playToServer(P5Payloads.P5Response.TYPE, P5Payloads.P5Response.CODEC, (msg, ctx) -> {});
    }
}
