package xyz.projectminecraft.anticheat.p5;

import net.minecraft.network.FriendlyByteBuf;
import net.minecraft.network.codec.StreamCodec;
import net.minecraft.network.protocol.common.custom.CustomPacketPayload;
import net.minecraft.resources.ResourceLocation;

/**
 * Кастомные пакеты канала P5. NeoForge 1.21.x: CustomPacketPayload + Type + StreamCodec,
 * регистрируются в RegisterPayloadHandlersEvent (см. P5Mod).
 *
 * ⚠️ Сигнатуры StreamCodec/CustomPacketPayload версионно-зависимы — сверь со своей NeoForge.
 */
final class P5Payloads {
    private P5Payloads() {}

    static final String MOD_ID = "pjmac";

    /** Сервер → клиент: случайный challenge (hex). */
    record P5Challenge(String nonce) implements CustomPacketPayload {
        static final Type<P5Challenge> TYPE =
                new Type<>(ResourceLocation.fromNamespaceAndPath(MOD_ID, "p5_challenge"));
        static final StreamCodec<FriendlyByteBuf, P5Challenge> CODEC = StreamCodec.of(
                (buf, msg) -> buf.writeUtf(msg.nonce(), 64),
                buf -> new P5Challenge(buf.readUtf(64)));

        @Override public Type<? extends CustomPacketPayload> type() { return TYPE; }
    }

    /** Клиент → сервер: proof = HMAC(challenge, accessToken) (hex). */
    record P5Response(String proof) implements CustomPacketPayload {
        static final Type<P5Response> TYPE =
                new Type<>(ResourceLocation.fromNamespaceAndPath(MOD_ID, "p5_response"));
        static final StreamCodec<FriendlyByteBuf, P5Response> CODEC = StreamCodec.of(
                (buf, msg) -> buf.writeUtf(msg.proof(), 128),
                buf -> new P5Response(buf.readUtf(128)));

        @Override public Type<? extends CustomPacketPayload> type() { return TYPE; }
    }
}
