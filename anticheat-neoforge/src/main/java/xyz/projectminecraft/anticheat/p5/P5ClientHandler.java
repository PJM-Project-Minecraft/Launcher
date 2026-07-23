package xyz.projectminecraft.anticheat.p5;

import net.minecraft.client.Minecraft;
import net.neoforged.neoforge.network.handling.IPayloadContext;

/**
 * КЛИЕНТСКАЯ сторона P5. Получил challenge → взял accessToken текущей игровой сессии →
 * посчитал proof = HMAC(challenge, accessToken) → отправил ответ серверу.
 *
 * accessToken тут — это токен сессии Minecraft (тот же, что лаунчер выдал через yggdrasil
 * и вшил в запуск), поэтому подлинный клиент считает proof, который сойдётся с backend.
 *
 * ⚠️ Класс грузится ТОЛЬКО на клиенте (Dist.CLIENT) — не тяни его на сервере.
 */
final class P5ClientHandler {
    private P5ClientHandler() {}

    static void onChallenge(P5Payloads.P5Challenge msg, IPayloadContext ctx) {
        String accessToken = Minecraft.getInstance().getUser().getAccessToken();
        String proof = P5Crypto.proof(msg.nonce(), accessToken);
        ctx.reply(new P5Payloads.P5Response(proof));
    }
}
