package xyz.projectminecraft.anticheat;

import java.lang.instrument.ClassFileTransformer;
import java.lang.instrument.Instrumentation;
import java.lang.management.ManagementFactory;
import java.net.URI;
import java.net.http.HttpClient;
import java.net.http.HttpRequest;
import java.net.http.HttpResponse;
import java.nio.file.Files;
import java.nio.file.Path;
import java.nio.file.Paths;
import java.security.ProtectionDomain;
import java.time.Duration;
import java.util.ArrayList;
import java.util.List;
import java.util.Locale;
import java.util.concurrent.CopyOnWriteArrayList;

/**
 * Игровой Java-агент античита (M3). Грузится в JVM Minecraft через -javaagent
 * (premain), подтверждает античит-handshake (confirm) и репортит подозрительные
 * классы/моды на бэкенд по launch-token. Зависимостей вне JDK нет.
 *
 * Параметры через -D: ac.token (launch-token), ac.url (базовый URL бэкенда).
 */
public final class Agent {

    private static final Duration TIMEOUT = Duration.ofSeconds(10);
    private static final long HEARTBEAT_PERIOD_MS = 30_000L;

    // Дефолтный seed маркеров известных чит-клиентов/модов (на случай недоступности
    // бэкенда). Актуальный набор тянется с сервера через /rules и атомарно заменяет
    // markers; ре-фетч — при изменении версии блэклиста (сигнал из heartbeat).
    private static final java.util.Set<String> DEFAULT_MARKERS = java.util.Set.of(
        "wurst", "meteorclient", "baritone", "xray", "killaura",
        "aimbot", "liquidbounce", "impactclient", "sigmaclient", "huzuni"
    );
    private static volatile java.util.Set<String> markers = DEFAULT_MARKERS;
    private static volatile long blacklistVersion = -1;

    private static volatile String token = "";
    private static volatile String baseUrl = "";
    private static volatile String kickFile = "";
    private static final HttpClient HTTP = HttpClient.newBuilder().connectTimeout(TIMEOUT).build();
    private static final List<String> reported = new CopyOnWriteArrayList<>();
    // Ограничитель потока детектов нелегальных имён (защита от флуда / FP-шторма).
    private static final java.util.concurrent.atomic.AtomicInteger illegalReports =
        new java.util.concurrent.atomic.AtomicInteger(0);
    private static final int MAX_ILLEGAL_REPORTS = 20;

    private Agent() {}

    public static void premain(String args, Instrumentation inst) {
        token = System.getProperty("ac.token", "");
        baseUrl = trimSlash(System.getProperty("ac.url", ""));
        kickFile = System.getProperty("ac.kickfile", "");
        if (token.isEmpty() || baseUrl.isEmpty()) {
            // Без параметров агент ничего не делает (не ломаем запуск).
            return;
        }

        // 0. Считываем состояние нативного JVMTI-агента (M4) из flag-файла.
        NativeState nativeState = readNativeState();
        if (!nativeState.present) {
            // Нативный слой не загрузился — анти-инжект отключён или обойдён. Низкая
            // severity: репортим, но НЕ кикаем (мог не подняться по легитимной причине).
            detect("tamper", "native-agent-missing", "native flag absent", 6);
        } else if (nativeState.debug) {
            detect("debugger", "debugger-attached", "TracerPid/IsDebuggerPresent", 6);
        }

        // 1. Подтверждаем защиту: отправляем proof о том, что агент реально загружен.
        confirm(inst, nativeState);

        // 1.5. Тянем актуальный блэклист с сервера (по launch-token) до скана, чтобы
        //      сканировать уже по полному набору сигнатур, а не только по seed.
        fetchRules();

        // 2. Сканируем уже загруженные классы и моды на маркеры читов.
        scanLoadedClasses(inst);
        scanModsDirectory();
        // 2.1. Чужой -javaagent/-agentpath в командной строке — ghost-клиент (его не ставил
        //      лаунчер: он сам собирает JVM-команду из серверного манифеста).
        scanJvmArgsForForeignAgents();

        // 3. Ловим классы, загружаемые позже (читы часто грузятся лениво).
        inst.addTransformer(new SuspectTransformer(), false);

        // 4. Поллер событий нативного агента (нелегальные имена классов и пр.):
        //    нативный ClassFileLoadHook пишет их в <flag>.events, мы пересылаем на бэкенд.
        String flag = System.getProperty("ac.native.flag", "");
        if (!flag.isEmpty()) {
            startEventPoller(flag + ".events");
        }

        // 5. Фоновый heartbeat — задел под realtime-контроль (M5).
        startHeartbeat();
    }

    /**
     * Имя класса нелегально, если содержит символ, который НЕ может появиться в имени,
     * сгенерированном настоящим компилятором: control-символы и пунктуацию. Инжекторы
     * дают классам такие имена («Naming: Illegal»), чтобы обходить детект по сигнатурам.
     *
     * ВАЖНО: JVM и Java легально допускают в идентификаторах ЛЮБЫЕ юникод-буквы, а не
     * только ASCII (см. Character.isJavaIdentifierPart). Легитимные моды этим пользуются:
     * напр. Axiom прячет пакет `com.moulberry.axiοm.utils.Authorization`, подменяя `o` на
     * греческий омикрон U+03BF. ASCII-only проверка ловила это как инъекцию → ложный кик.
     * Поэтому пропускаем валидные идентификатор-символы (вкл. юникод) и разделители `.`/`/`,
     * а нелегальными считаем только control-символы и прочую пунктуацию.
     */
    private static boolean isIllegalClassName(String name) {
        if (name == null || name.isEmpty()) {
            return false;
        }
        for (int i = 0; i < name.length(); i++) {
            char c = name.charAt(i);
            // Control-символы (вкл. \t \n) — подпись инъектора, легальный компилятор их не даёт.
            // Проверяем ПЕРВЫМ: isJavaIdentifierPart() считает ignorable-control частью идентификатора.
            if (Character.isISOControl(c)) {
                return true;
            }
            // Разделители внутреннего имени класса.
            if (c == '/' || c == '.') {
                continue;
            }
            // Буквы/цифры/_/$ — в т.ч. юникодные (валидный идентификатор JVM).
            if (Character.isJavaIdentifierPart(c)) {
                continue;
            }
            return true;
        }
        return false;
    }

    private static void reportIllegalName(String name, String source) {
        if (illegalReports.incrementAndGet() > MAX_ILLEGAL_REPORTS) {
            return; // достаточно сигнала — не флудим бэкенд
        }
        String key = "illegal:" + name;
        if (reported.contains(key)) {
            return;
        }
        reported.add(key);
        detect("inject", "illegal-class-name", source + ":" + name);
    }

    private static void confirm(Instrumentation inst, NativeState ns) {
        // Attestation-proof (P3): эхо challenge из токена, self-hash нашего jar (сверяется
        // с манифестом), факт присутствия нативного слоя и отсутствие чужих агентов.
        String challenge = System.getProperty("ac.challenge", "");
        String proof = "{"
            + "\"challenge\":\"" + escape(challenge) + "\","
            + "\"agentSha256\":\"" + escape(selfSha256()) + "\","
            + "\"nativePresent\":" + ns.present + ","
            + "\"foreignAgents\":" + hasForeignAgents() + ","
            + "\"loadedClasses\":" + inst.getAllLoadedClasses().length + ","
            + "\"javaVersion\":\"" + escape(System.getProperty("java.version", "")) + "\","
            + "\"native\":{\"present\":" + ns.present + ",\"debug\":" + ns.debug
                + ",\"classhook\":" + ns.classhook + "},"
            + "\"jvmArgs\":" + jsonStringArray(ManagementFactory.getRuntimeMXBean().getInputArguments())
            + "}";
        String body = "{\"launchToken\":\"" + escape(token) + "\",\"proof\":" + proof + "}";
        post("/api/anticheat/handshake/confirm", body);
    }

    /** SHA-256 собственного jar (через CodeSource) — для сверки с манифестом на сервере. */
    private static String selfSha256() {
        try {
            java.security.CodeSource cs = Agent.class.getProtectionDomain().getCodeSource();
            if (cs == null || cs.getLocation() == null) {
                return "";
            }
            byte[] data = Files.readAllBytes(Paths.get(cs.getLocation().toURI()));
            byte[] h = java.security.MessageDigest.getInstance("SHA-256").digest(data);
            StringBuilder sb = new StringBuilder(h.length * 2);
            for (byte b : h) {
                sb.append(String.format("%02x", b));
            }
            return sb.toString();
        } catch (Exception e) {
            return "";
        }
    }

    /** Состояние нативного JVMTI-агента, прочитанное из flag-файла. */
    private static final class NativeState {
        boolean present;
        boolean debug;
        boolean classhook;
    }

    private static NativeState readNativeState() {
        NativeState s = new NativeState();
        String path = System.getProperty("ac.native.flag", "");
        if (path.isEmpty()) {
            return s;
        }
        try {
            for (String line : Files.readAllLines(Paths.get(path))) {
                int eq = line.indexOf('=');
                if (eq <= 0) {
                    continue;
                }
                String key = line.substring(0, eq).trim();
                boolean val = "1".equals(line.substring(eq + 1).trim());
                switch (key) {
                    case "present" -> s.present = val;
                    case "debug" -> s.debug = val;
                    case "classhook" -> s.classhook = val;
                    default -> { /* игнор неизвестных ключей */ }
                }
            }
        } catch (Exception ignored) {
            // Файл недоступен → present остаётся false → детект native-agent-missing.
        }
        return s;
    }

    private static void scanLoadedClasses(Instrumentation inst) {
        // ВАЖНО: здесь НЕ проверяем нелегальные имена. getAllLoadedClasses() включает
        // классы-массивы (имена вида "[Lpkg.Class;") — это легальные дескрипторы JVM,
        // а не инъекция. Инъекция происходит в рантайме и ловится в трансформере и
        // нативном ClassFileLoadHook (туда массивы-дескрипторы не попадают).
        for (Class<?> c : inst.getAllLoadedClasses()) {
            String name = c.getName();
            if (name != null && !c.isArray()) {
                checkAndReport("class", name);
            }
        }
    }

    private static void scanModsDirectory() {
        Path mods = Paths.get("mods");
        if (!Files.isDirectory(mods)) {
            return;
        }
        try (var stream = Files.list(mods)) {
            stream.filter(p -> p.toString().toLowerCase(Locale.ROOT).endsWith(".jar"))
                  .forEach(p -> checkAndReport("jar", p.getFileName().toString()));
        } catch (Exception ignored) {
            // Скан best-effort: ошибки чтения каталога не должны ронять игру.
        }
    }

    // Имена наших легитимных агентов в командной строке (всё остальное -javaagent/-agentpath
    // = посторонняя инъекция, т.к. JVM-команду собирает лаунчер из серверного манифеста).
    private static final String[] OWN_AGENTS = {
        "authlib-injector", "anticheat-agent", "libanticheat", "anticheat.dll"
    };

    /** true, если в аргументах JVM есть посторонний -javaagent/-agentpath (для proof). */
    private static boolean hasForeignAgents() {
        try {
            for (String arg : ManagementFactory.getRuntimeMXBean().getInputArguments()) {
                String low = arg.toLowerCase(Locale.ROOT);
                if (!low.startsWith("-javaagent:") && !low.startsWith("-agentpath:")) {
                    continue;
                }
                boolean own = false;
                for (String name : OWN_AGENTS) {
                    if (low.contains(name)) {
                        own = true;
                        break;
                    }
                }
                if (!own) {
                    return true;
                }
            }
        } catch (Exception ignored) {
            // best-effort
        }
        return false;
    }

    /** Ищет в аргументах JVM посторонние -javaagent/-agentpath (ghost-клиент). */
    private static void scanJvmArgsForForeignAgents() {
        try {
            for (String arg : ManagementFactory.getRuntimeMXBean().getInputArguments()) {
                String low = arg.toLowerCase(Locale.ROOT);
                if (!low.startsWith("-javaagent:") && !low.startsWith("-agentpath:")) {
                    continue;
                }
                boolean own = false;
                for (String name : OWN_AGENTS) {
                    if (low.contains(name)) {
                        own = true;
                        break;
                    }
                }
                if (!own) {
                    detect("inject", "foreign-agent", arg); // server severity inject=9 → kick
                }
            }
        } catch (Exception ignored) {
            // best-effort
        }
    }

    /** Сверяет имя с маркерами и, при совпадении, шлёт детект (без дублей). */
    private static void checkAndReport(String kind, String name) {
        String lower = name.toLowerCase(Locale.ROOT);
        for (String marker : markers) {
            if (lower.contains(marker)) {
                String key = kind + ":" + marker;
                if (reported.contains(key)) {
                    return;
                }
                reported.add(key);
                detect(kind, marker, name);
                return;
            }
        }
    }

    private static void detect(String type, String signature, String detail) {
        detect(type, signature, detail, 8);
    }

    private static void detect(String type, String signature, String detail, int severity) {
        String body = "{"
            + "\"launchToken\":\"" + escape(token) + "\","
            + "\"source\":\"java\","
            + "\"type\":\"" + escape(type) + "\","
            + "\"signature\":\"" + escape(signature) + "\","
            + "\"severity\":" + severity + ","
            + "\"details\":{\"name\":\"" + escape(detail) + "\"}"
            + "}";
        String resp = postRead("/api/anticheat/detect", body);
        // Бэкенд решает реакцию: при kick убиваем игру и оставляем причину лаунчеру.
        if (resp != null && resp.contains("\"action\":\"kick\"")) {
            kickGame(signature);
        }
    }

    /** Пишет причину кика лаунчеру и немедленно убивает JVM (halt, без shutdown-хуков). */
    private static void kickGame(String reason) {
        try {
            if (!kickFile.isEmpty()) {
                Files.write(Paths.get(kickFile),
                    ("reason=" + reason + "\n").getBytes(java.nio.charset.StandardCharsets.UTF_8));
            }
        } catch (Exception ignored) {
            // даже если файл не записался — всё равно убиваем игру
        }
        System.err.println("[anticheat] kick: " + reason + " — закрываю игру");
        Runtime.getRuntime().halt(66); // 66 = код выхода «закрыто античитом»
    }

    // Поллер событий нативного агента: каждые 2с дочитывает новые строки <flag>.events
    // и пересылает как детекты. Нативный hook видит инъекции, недоступные Java (hidden,
    // Unsafe), и его сложнее снять, чем Java-трансформер.
    private static void startEventPoller(String eventsPath) {
        Thread t = new Thread(() -> {
            long offset = 0;
            Path path = Paths.get(eventsPath);
            while (true) {
                try {
                    Thread.sleep(2000);
                } catch (InterruptedException e) {
                    return;
                }
                try {
                    if (!Files.exists(path)) {
                        continue;
                    }
                    byte[] all = Files.readAllBytes(path);
                    if (all.length <= offset) {
                        continue;
                    }
                    String chunk = new String(all, (int) offset, (int) (all.length - offset),
                        java.nio.charset.StandardCharsets.UTF_8);
                    offset = all.length;
                    for (String line : chunk.split("\n")) {
                        line = line.trim();
                        if (line.isEmpty()) {
                            continue;
                        }
                        // Формат строки: "<type>\t<name>".
                        int tab = line.indexOf('\t');
                        String type = tab > 0 ? line.substring(0, tab) : "inject";
                        String name = tab > 0 ? line.substring(tab + 1) : line;
                        // Точные сигналы (нелегальные имена, маркеры классов) → kick;
                        // эвристики guard-потока (новый модуль, ld-preload, late-debug) —
                        // report-only (обкатка против ложных срабатываний). Severity всё равно
                        // назначает сервер по detect-type: inject=9(kick), debugger=6, прочее=5.
                        switch (type) {
                            case "illegal-class-name" -> reportIllegalName(name, "native");
                            case "debugger-runtime" -> detect("debugger", "debugger-runtime", "native:" + name);
                            case "module-unknown" -> detect("module-unknown", "module-unknown", "native:" + name);
                            case "ld-preload" -> detect("ld-preload", "ld-preload", "native:" + name);
                            default -> detect("inject", type, "native:" + name); // маркеры читов в именах классов
                        }
                    }
                } catch (Exception ignored) {
                    // best-effort
                }
            }
        }, "anticheat-native-events");
        t.setDaemon(true);
        t.start();
    }

    private static void startHeartbeat() {
        Thread t = new Thread(() -> {
            while (true) {
                try {
                    Thread.sleep(HEARTBEAT_PERIOD_MS);
                } catch (InterruptedException e) {
                    return;
                }
                String resp = postRead("/api/anticheat/heartbeat",
                    "{\"launchToken\":\"" + escape(token) + "\"}");
                if (resp == null) {
                    continue; // сеть нестабильна — не убиваем игру (enforcement даёт сервер)
                }
                // Сервер погасил сессию (detect в другой сессии/reaper) → закрываем игру.
                if (resp.contains("\"action\":\"kick\"")) {
                    kickGame("session-revoked");
                    return;
                }
                // Версия блэклиста изменилась → подтягиваем свежие правила.
                long v = parseLongField(resp, "blacklistVersion");
                if (v >= 0 && v != blacklistVersion) {
                    fetchRules();
                }
            }
        }, "anticheat-heartbeat");
        t.setDaemon(true);
        t.start();
    }

    /** Тянет блэклист с сервера (/rules по launch-token) и атомарно обновляет markers. */
    private static void fetchRules() {
        try {
            HttpRequest req = HttpRequest.newBuilder()
                .uri(URI.create(baseUrl + "/api/anticheat/rules"))
                .timeout(TIMEOUT)
                .header("X-Launch-Token", token)
                .GET()
                .build();
            HttpResponse<String> resp = HTTP.send(req, HttpResponse.BodyHandlers.ofString());
            if (resp.statusCode() != 200) {
                return;
            }
            String body = resp.body();
            java.util.Set<String> patterns = parsePatterns(body);
            if (!patterns.isEmpty()) {
                // Объединяем с дефолтным seed, чтобы усечённый/частичный блэклист не ослаблял детект.
                java.util.Set<String> merged = new java.util.HashSet<>(DEFAULT_MARKERS);
                merged.addAll(patterns);
                markers = merged;
            }
            long v = parseLongField(body, "version");
            if (v >= 0) {
                blacklistVersion = v;
            }
        } catch (Exception ignored) {
            // best-effort: при ошибке остаёмся на текущем наборе markers
        }
    }

    // Минимальный парсинг контролируемого JSON-ответа сервера (без сторонних библиотек).

    /** Читает целочисленное поле "field": N. -1, если не найдено/не число. */
    private static long parseLongField(String body, String field) {
        String key = "\"" + field + "\":";
        int i = body.indexOf(key);
        if (i < 0) {
            return -1;
        }
        i += key.length();
        while (i < body.length() && Character.isWhitespace(body.charAt(i))) {
            i++;
        }
        int j = i;
        if (j < body.length() && body.charAt(j) == '-') {
            j++;
        }
        while (j < body.length() && Character.isDigit(body.charAt(j))) {
            j++;
        }
        try {
            return Long.parseLong(body.substring(i, j));
        } catch (Exception e) {
            return -1;
        }
    }

    /** Извлекает все значения "pattern":"..." из ответа /rules (lowercase). */
    private static java.util.Set<String> parsePatterns(String body) {
        java.util.Set<String> out = new java.util.HashSet<>();
        String key = "\"pattern\":\"";
        int idx = 0;
        while ((idx = body.indexOf(key, idx)) >= 0) {
            idx += key.length();
            StringBuilder sb = new StringBuilder();
            int end = idx;
            while (end < body.length()) {
                char c = body.charAt(end);
                if (c == '\\' && end + 1 < body.length()) {
                    sb.append(body.charAt(end + 1));
                    end += 2;
                    continue;
                }
                if (c == '"') {
                    break;
                }
                sb.append(c);
                end++;
            }
            String pat = sb.toString().trim().toLowerCase(Locale.ROOT);
            if (!pat.isEmpty()) {
                out.add(pat);
            }
            idx = end;
        }
        return out;
    }

    private static void post(String path, String json) {
        postRead(path, json);
    }

    /** POST с чтением тела ответа (нужно для action из /detect). null при ошибке. */
    private static String postRead(String path, String json) {
        try {
            HttpRequest req = HttpRequest.newBuilder()
                .uri(URI.create(baseUrl + path))
                .timeout(TIMEOUT)
                .header("Content-Type", "application/json")
                .POST(HttpRequest.BodyPublishers.ofString(json))
                .build();
            return HTTP.send(req, HttpResponse.BodyHandlers.ofString()).body();
        } catch (Exception ignored) {
            // Сеть нестабильна — не роняем игру; enforcement обеспечивает сервер.
            return null;
        }
    }

    /** Транформер: не меняет байткод, лишь инспектирует имена загружаемых классов. */
    private static final class SuspectTransformer implements ClassFileTransformer {
        @Override
        public byte[] transform(ClassLoader loader, String className, Class<?> classBeingRedefined,
                                ProtectionDomain protectionDomain, byte[] classfileBuffer) {
            if (className != null) {
                // Проверяем сырое внутреннее имя (со слэшами) на нелегальные символы.
                if (isIllegalClassName(className)) {
                    reportIllegalName(className, "transform");
                }
                checkAndReport("class", className.replace('/', '.'));
            }
            return null; // null = байткод не изменён
        }
    }

    private static String trimSlash(String s) {
        if (s == null) {
            return "";
        }
        while (s.endsWith("/")) {
            s = s.substring(0, s.length() - 1);
        }
        return s;
    }

    private static String jsonStringArray(List<String> items) {
        List<String> parts = new ArrayList<>(items.size());
        for (String item : items) {
            parts.add("\"" + escape(item) + "\"");
        }
        return "[" + String.join(",", parts) + "]";
    }

    private static String escape(String s) {
        if (s == null) {
            return "";
        }
        StringBuilder sb = new StringBuilder(s.length() + 8);
        for (int i = 0; i < s.length(); i++) {
            char c = s.charAt(i);
            switch (c) {
                case '"' -> sb.append("\\\"");
                case '\\' -> sb.append("\\\\");
                case '\n' -> sb.append("\\n");
                case '\r' -> sb.append("\\r");
                case '\t' -> sb.append("\\t");
                default -> {
                    if (c < 0x20) {
                        sb.append(String.format("\\u%04x", (int) c));
                    } else {
                        sb.append(c);
                    }
                }
            }
        }
        return sb.toString();
    }
}
