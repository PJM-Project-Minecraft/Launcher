package xyz.projectminecraft.anticheat.p5;

import com.mojang.logging.LogUtils;
import net.minecraft.server.level.ServerPlayer;
import net.neoforged.bus.api.IEventBus;
import org.slf4j.Logger;
import net.neoforged.fml.common.Mod;
import net.neoforged.neoforge.common.NeoForge;
import net.neoforged.neoforge.event.entity.player.PlayerEvent;
import net.neoforged.neoforge.event.server.ServerStartedEvent;
import net.neoforged.neoforge.event.server.ServerStoppingEvent;
import net.neoforged.neoforge.network.event.RegisterPayloadHandlersEvent;
import net.neoforged.neoforge.network.registration.PayloadRegistrar;

/**
 * Точка входа мода P5. Регистрирует канал challenge/response и вешает обработчик входа
 * игрока. Мод грузится на ОБЕИХ сторонах; серверная логика активна только при заданном
 * ANTICHEAT_P5_SECRET (см. P5Config), клиентская отвечает на challenge всегда.
 *
 * ⚠️ Network-API NeoForge версионно-зависим (registrar.playToClient/playToServer,
 * RegisterPayloadHandlersEvent) — сверь со своей версией. P5ClientHandler ссылается на
 * client-only класс Minecraft: на сервере класс грузится, но onChallenge там не вызывается
 * (ленивое разрешение). Если твоя сборка ругается — регистрируй клиентский обработчик под
 * Dist.CLIENT отдельно.
 */
@Mod(P5Payloads.MOD_ID)
public final class P5Mod {
    /** Общий логгер мода — на консоли сервера с префиксом [P5], чтобы обкатку было видно. */
    static final Logger LOG = LogUtils.getLogger();

    public P5Mod(IEventBus modBus) {
        modBus.addListener(P5Mod::registerPayloads);
        NeoForge.EVENT_BUS.addListener(P5Mod::onPlayerJoin);
        NeoForge.EVENT_BUS.addListener(P5Mod::onServerStarted);
        NeoForge.EVENT_BUS.addListener(P5Mod::onServerStopping);
    }

    private static void registerPayloads(RegisterPayloadHandlersEvent event) {
        PayloadRegistrar registrar = event.registrar("1");
        registrar.playToClient(P5Payloads.P5Challenge.TYPE, P5Payloads.P5Challenge.CODEC,
                P5ClientHandler::onChallenge);
        registrar.playToServer(P5Payloads.P5Response.TYPE, P5Payloads.P5Response.CODEC,
                P5ServerHandler::onResponse);
    }

    /** Опрос отзывов доступа: бэкенд решает, кого кикнуть, сервер исполняет. */
    private static void onServerStarted(ServerStartedEvent event) {
        if (P5Config.active()) {
            LOG.info("[P5] активен: вход-хэндшейк + опрос отзывов каждые 25с, бэкенд {}", P5Config.API);
        } else {
            LOG.warn("[P5] ВЫКЛЮЧЕН: пустой ANTICHEAT_P5_SECRET в env сервера — защита не работает");
        }
        P5RevokePoller.start(event.getServer());
    }

    private static void onServerStopping(ServerStoppingEvent event) {
        P5RevokePoller.stop();
    }

    private static void onPlayerJoin(PlayerEvent.PlayerLoggedInEvent event) {
        if (event.getEntity() instanceof ServerPlayer player) {
            P5ServerHandler.onPlayerJoin(player);
        }
    }
}
