package xyz.projectminecraft.anticheat.p5;

import javax.crypto.Mac;
import javax.crypto.spec.SecretKeySpec;
import java.nio.charset.StandardCharsets;

/**
 * HMAC-SHA256(challenge) на ключе accessToken → hex (lowercase). ДОЛЖНО совпадать
 * байт-в-байт с бэкендом (backend/internal/anticheat/p5.go: p5Proof):
 *   mac := hmac.New(sha256.New, []byte(accessToken)); mac.Write([]byte(challenge))
 * — тот же ключ, те же данные, hex.EncodeToString. UTF-8 обе стороны.
 */
final class P5Crypto {
    private P5Crypto() {}

    static String proof(String challenge, String accessToken) {
        try {
            Mac mac = Mac.getInstance("HmacSHA256");
            mac.init(new SecretKeySpec(accessToken.getBytes(StandardCharsets.UTF_8), "HmacSHA256"));
            byte[] out = mac.doFinal(challenge.getBytes(StandardCharsets.UTF_8));
            StringBuilder sb = new StringBuilder(out.length * 2);
            for (byte b : out) sb.append(Character.forDigit((b >> 4) & 0xf, 16)).append(Character.forDigit(b & 0xf, 16));
            return sb.toString();
        } catch (Exception e) {
            return ""; // невозможность посчитать → пустой proof → бэкенд отвергнет
        }
    }
}
