package xyz.projectminecraft.anticheat.p5;

/**
 * Конфиг P5 для СЕРВЕРНОЙ стороны. Секрет и URL бэкенда берём из окружения игрового
 * сервера (не хардкодим). Задай в env процесса сервера:
 *   ANTICHEAT_P5_SECRET  — тот же, что в прод .env бэкенда (openssl rand -hex 32)
 *   LAUNCHER_API         — база бэкенда, напр. https://launcher.likonchik.xyz
 * Пустой секрет → P5 на сервере не активен (мод не челленджит), это безопасный дефолт.
 */
final class P5Config {
    private P5Config() {}

    static final String SECRET = System.getenv().getOrDefault("ANTICHEAT_P5_SECRET", "");
    static final String API = trimSlash(System.getenv().getOrDefault("LAUNCHER_API", "https://launcher.likonchik.xyz"));

    /** Сколько ждать ответ клиента, прежде чем считать proof пустым (мс). */
    static final long RESPONSE_TIMEOUT_MS = 8_000L;
    /** Таймаут HTTP-запроса к бэкенду (мс). */
    static final int HTTP_TIMEOUT_MS = 5_000;

    static boolean active() {
        return !SECRET.isEmpty();
    }

    private static String trimSlash(String s) {
        while (s.endsWith("/")) s = s.substring(0, s.length() - 1);
        return s;
    }
}
