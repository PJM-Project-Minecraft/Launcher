package xyz.projectminecraft.anticheat.p5;

import com.mojang.logging.LogUtils;
import net.minecraft.server.level.ServerPlayer;
import net.neoforged.bus.api.IEventBus;
import net.neoforged.fml.common.Mod;
import net.neoforged.neoforge.common.NeoForge;
import net.neoforged.neoforge.event.entity.player.PlayerEvent;
import net.neoforged.neoforge.event.server.ServerStartedEvent;
import net.neoforged.neoforge.event.server.ServerStoppingEvent;
import net.neoforged.neoforge.network.event.RegisterPayloadHandlersEvent;
import net.neoforged.neoforge.network.registration.PayloadRegistrar;
import org.slf4j.Logger;

/**
 * СЕРВЕРНАЯ точка входа мода P5 (jar с classifier=server, на игровой сервер). Содержит
 * серверную логику: выдачу challenge на входе, верификацию proof через бэкенд, опрос
 * отзывов доступа. Клиентских классов (Minecraft, P5ClientHandler, P5Crypto) в этом jar НЕТ.
 *
 * modId тот же "pjmac", что у клиентского jar — namespace сетевого канала совпадает,
 * стороны связываются. Два @Mod("pjmac") в одном рантайме не встречаются (по jar на сторону).
 */
@Mod(P5Payloads.MOD_ID)
public final class P5ModServer {
    /** Логгер мода — на консоли сервера с префиксом [P5], чтобы обкатку было видно. */
    static final Logger LOG = LogUtils.getLogger();

    public P5ModServer(IEventBus modBus) {
        modBus.addListener(P5ModServer::registerPayloads);
        NeoForge.EVENT_BUS.addListener(P5ModServer::onPlayerJoin);
        NeoForge.EVENT_BUS.addListener(P5ModServer::onServerStarted);
        NeoForge.EVENT_BUS.addListener(P5ModServer::onServerStopping);
    }

    private static void registerPayloads(RegisterPayloadHandlersEvent event) {
        PayloadRegistrar registrar = event.registrar("1");
        // Сервер ОТПРАВЛЯЕТ challenge — тип надо зарегистрировать, но сервер его не принимает
        // (noop-хендлер, на сервере не вызывается).
        registrar.playToClient(P5Payloads.P5Challenge.TYPE, P5Payloads.P5Challenge.CODEC, (msg, ctx) -> {});
        // Сервер ПРИНИМАЕТ ответ клиента.
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
