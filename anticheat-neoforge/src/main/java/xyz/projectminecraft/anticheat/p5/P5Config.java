package xyz.projectminecraft.anticheat.p5;

/**
 * Конфиг P5 для СЕРВЕРНОЙ стороны. Секрет и URL бэкенда берём из окружения игрового
 * сервера (не хардкодим). Задай в env процесса сервера:
 *   ANTICHEAT_P5_SECRET  — тот же, что в прод .env бэкенда (openssl rand -hex 32)
 *   ANTICHEAT_P5_ENFORCE — тот же true/false, что на бэкенде
 *   LAUNCHER_API         — база бэкенда, напр. https://launcher.likonchik.xyz
 * Пустой секрет → P5 на сервере не активен (мод не челленджит), это безопасный дефолт.
 */
final class P5Config {
    private P5Config() {}

    static final String SECRET = System.getenv().getOrDefault("ANTICHEAT_P5_SECRET", "");
    static final boolean ENFORCE = Boolean.parseBoolean(System.getenv().getOrDefault("ANTICHEAT_P5_ENFORCE", "false"));
    static final String API = trimSlash(System.getenv().getOrDefault("LAUNCHER_API", "https://launcher.likonchik.xyz"));

    /** Сколько ждать ответ клиента на одну попытку, прежде чем перевыслать challenge (мс). */
    static final long RESPONSE_TIMEOUT_MS = 15_000L;
    /** Сколько попыток всего. Окно ответа = RESPONSE_TIMEOUT_MS × CHALLENGE_ATTEMPTS.
     *  8с на входе не хватало: challenge приходит на PlayerLoggedInEvent, когда клиент
     *  ещё грузит мир и качает чанки — на слабом канале ответ опаздывал, мод верифицировал
     *  пустой proof и в enforce кикал честного игрока. Читер, не реализующий канал, не
     *  ответит и за 45с, а отзыв доступа опрашивается каждые 25с. */
    static final int CHALLENGE_ATTEMPTS = 3;
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
