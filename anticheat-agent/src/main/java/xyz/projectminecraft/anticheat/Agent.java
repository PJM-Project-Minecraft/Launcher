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

    // Правило блэклиста: паттерн + способ матча (substring|exact|word|regex|hash) + hash
    // (SHA-256 байткода для match_type=hash). Дефолтный seed — substring по именам;
    // актуальный набор тянется с сервера через /rules и атомарно заменяет rules.
    private record Rule(String pattern, String matchType, String hash) {}

    private static List<Rule> defaultRules() {
        List<Rule> r = new ArrayList<>();
        for (String m : DEFAULT_MARKERS) {
            r.add(new Rule(m, "substring", ""));
        }
        return r;
    }

    private static volatile List<Rule> rules = defaultRules();
    private static final java.util.Map<String, java.util.regex.Pattern> REGEX_CACHE =
        new java.util.concurrent.ConcurrentHashMap<>();
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
            return sha256hex(Files.readAllBytes(Paths.get(cs.getLocation().toURI())));
        } catch (Exception e) {
            return "";
        }
    }

    /** SHA-256 байтов в hex (нижний регистр). Пустая строка при ошибке. */
    private static String sha256hex(byte[] data) {
        try {
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

    /** Сверяет имя с правилами блэклиста по их match_type и, при совпадении, шлёт детект. */
    private static void checkAndReport(String kind, String name) {
        String lower = name.toLowerCase(Locale.ROOT);
        for (Rule rule : rules) {
            if (ruleMatches(rule, lower)) {
                String key = kind + ":" + rule.pattern();
                if (reported.contains(key)) {
                    return;
                }
                reported.add(key);
                detect(kind, rule.pattern(), name);
                return;
            }
        }
    }

    /** Применяет match_type правила к имени (нижний регистр). hash — про байткод, не имя (Фаза 2). */
    private static boolean ruleMatches(Rule rule, String lower) {
        String p = rule.pattern();
        if (p.isEmpty()) {
            return false;
        }
        switch (rule.matchType()) {
            case "exact":
                return lower.equals(p);
            case "word":
                return matchesWord(lower, p);
            case "regex":
                return matchesRegex(p, lower);
            case "hash":
                return false;
            default: // substring (вкл. пустой match_type)
                return lower.contains(p);
        }
    }

    /** true, если needle встречается в haystack как отдельное слово (границы — края/не-слово). */
    private static boolean matchesWord(String haystack, String needle) {
        int nlen = needle.length();
        if (nlen == 0 || nlen > haystack.length()) {
            return false;
        }
        int from = 0;
        int i;
        while ((i = haystack.indexOf(needle, from)) >= 0) {
            boolean leftOk = i == 0 || !isWordChar(haystack.charAt(i - 1));
            int endPos = i + nlen;
            boolean rightOk = endPos == haystack.length() || !isWordChar(haystack.charAt(endPos));
            if (leftOk && rightOk) {
                return true;
            }
            from = i + 1;
        }
        return false;
    }

    private static boolean isWordChar(char c) {
        return Character.isLetterOrDigit(c) || c == '_';
    }

    private static boolean matchesRegex(String pattern, String lower) {
        try {
            return REGEX_CACHE.computeIfAbsent(pattern, java.util.regex.Pattern::compile)
                .matcher(lower).find();
        } catch (Exception e) {
            return false; // невалидный regex (сервер валидирует при сохранении) — не матчим
        }
    }

    private static boolean hasHashRules() {
        for (Rule r : rules) {
            if ("hash".equals(r.matchType()) && !r.hash().isEmpty()) {
                return true;
            }
        }
        return false;
    }

    /** Считает SHA-256 байткода и сверяет с hash-правилами. Бьёт обфускацию имён: хеш не
     *  зависит от имени класса. Хеширует только при наличии hash-правил (иначе зря грузим
     *  каждый класс на старте). */
    private static void checkClassHash(byte[] classfileBuffer, String className) {
        if (classfileBuffer == null || classfileBuffer.length == 0 || !hasHashRules()) {
            return;
        }
        matchHash(sha256hex(classfileBuffer), className);
    }

    /** Сверяет готовый hash с hash-правилами; при совпадении — детект (без дублей). */
    private static void matchHash(String hash, String className) {
        if (hash == null || hash.isEmpty()) {
            return;
        }
        for (Rule r : rules) {
            if ("hash".equals(r.matchType()) && hash.equalsIgnoreCase(r.hash())) {
                String key = "classhash:" + hash;
                if (reported.contains(key)) {
                    return;
                }
                reported.add(key);
                detectHash(className, hash);
                return;
            }
        }
    }

    /** Детект по hash байткода: сервер берёт severity из hash-сигнатуры (confidence hard). */
    private static void detectHash(String className, String hash) {
        String name = (className == null || className.isEmpty()) ? hash : className;
        String body = "{"
            + "\"launchToken\":\"" + escape(token) + "\","
            + "\"source\":\"java\","
            + "\"type\":\"class\","
            + "\"signature\":\"" + escape(name) + "\","
            + "\"severity\":9,"
            + "\"details\":{\"name\":\"" + escape(name) + "\",\"hash\":\"" + escape(hash) + "\"}"
            + "}";
        String resp = postRead("/api/anticheat/detect", body);
        if (resp != null && resp.contains("\"action\":\"kick\"")) {
            kickGame("class-hash:" + name);
        }
    }

    private static boolean hasStringRules() {
        for (Rule r : rules) {
            if ("string-literal".equals(r.matchType()) && !r.pattern().isEmpty()) {
                return true;
            }
        }
        return false;
    }

    /** Парсит constant pool класса и возвращает все CONSTANT_Utf8-строки. Best-effort:
     *  при первой аномалии формата возвращает уже собранное. */
    private static List<String> extractStrings(byte[] buf) {
        List<String> out = new ArrayList<>();
        if (buf == null || buf.length < 10) {
            return out;
        }
        if ((buf[0] & 0xff) != 0xCA || (buf[1] & 0xff) != 0xFE
                || (buf[2] & 0xff) != 0xBA || (buf[3] & 0xff) != 0xBE) {
            return out;
        }
        int cpCount = ((buf[8] & 0xff) << 8) | (buf[9] & 0xff);
        int pos = 10;
        for (int i = 1; i < cpCount && pos < buf.length; i++) {
            int tag = buf[pos++] & 0xff;
            switch (tag) {
                case 1: { // Utf8
                    if (pos + 2 > buf.length) {
                        return out;
                    }
                    int len = ((buf[pos] & 0xff) << 8) | (buf[pos + 1] & 0xff);
                    pos += 2;
                    if (pos + len > buf.length) {
                        return out;
                    }
                    out.add(new String(buf, pos, len, java.nio.charset.StandardCharsets.UTF_8));
                    pos += len;
                    break;
                }
                case 7: case 8: case 16: case 19: case 20: pos += 2; break;
                case 15: pos += 3; break;
                case 3: case 4: case 9: case 10: case 11: case 12: case 17: case 18: pos += 4; break;
                case 5: case 6: pos += 8; i++; break; // Long/Double — два слота CP
                default: return out; // неизвестный tag — дальше парсить небезопасно
            }
        }
        return out;
    }

    /** Ищет сигнатурные строки в constant pool класса. Устойчивее hash к рекомпиляции:
     *  имена/URL чита в строках переживают пересборку. Парсит CP только при наличии правил. */
    private static void checkClassStrings(byte[] buf, String className) {
        if (buf == null || buf.length == 0 || !hasStringRules()) {
            return;
        }
        List<String> strings = null;
        for (Rule r : rules) {
            if (!"string-literal".equals(r.matchType()) || r.pattern().isEmpty()) {
                continue;
            }
            if (strings == null) {
                strings = extractStrings(buf);
            }
            for (String s : strings) {
                if (s.toLowerCase(Locale.ROOT).contains(r.pattern())) {
                    String key = "strlit:" + r.pattern();
                    if (reported.contains(key)) {
                        return;
                    }
                    reported.add(key);
                    // Совпавший паттерн как signature — сервер exact-сверит (severity hard).
                    detect("class", r.pattern(), "string-literal:" + className);
                    return;
                }
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
        final Path path = Paths.get(eventsPath);
        final long[] offset = {0}; // переживает итерации (тело — Runnable без своего состояния)
        runResilient("anticheat-native-events", 2000, () -> pollNativeEvents(path, offset));
    }

    /** Один проход поллера: дочитывает новые строки <flag>.events и шлёт как детекты. */
    private static void pollNativeEvents(Path path, long[] offset) {
        try {
            if (!Files.exists(path)) {
                return;
            }
            byte[] all = Files.readAllBytes(path);
            if (all.length <= offset[0]) {
                return;
            }
            String chunk = new String(all, (int) offset[0], (int) (all.length - offset[0]),
                java.nio.charset.StandardCharsets.UTF_8);
            offset[0] = all.length;
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
                    case "class-hash" -> {
                        // Нативка хеширует bootstrap-классы, недоступные Java-трансформеру:
                        // "class-hash\t<sha256>\t<className>". Сверяем с hash-правилами.
                        int t = name.indexOf('\t');
                        String h = (t > 0 ? name.substring(0, t) : name).trim().toLowerCase(Locale.ROOT);
                        String cls = t > 0 ? name.substring(t + 1) : "";
                        matchHash(h, cls);
                    }
                    default -> detect("inject", type, "native:" + name); // маркеры читов в именах классов
                }
            }
        } catch (Exception ignored) {
            // best-effort: ошибка чтения каталога не должна ронять поллер
        }
    }

    private static void startHeartbeat() {
        runResilient("anticheat-heartbeat", HEARTBEAT_PERIOD_MS, Agent::heartbeatOnce);
    }

    /** Один цикл heartbeat: пингует сервер, реагирует на kick и смену версии блэклиста. */
    private static void heartbeatOnce() {
        String resp = postRead("/api/anticheat/heartbeat",
            "{\"launchToken\":\"" + escape(token) + "\"}");
        if (resp == null) {
            return; // сеть нестабильна — не убиваем игру (enforcement даёт сервер)
        }
        // Сервер погасил сессию (detect в другой сессии) → закрываем игру.
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

    /**
     * Запускает неубиваемый daemon-цикл: тело выполняется каждые periodMs снова и снова,
     * а ЛЮБОЙ Throwable (включая Error и interrupt) поглощается и НЕ убивает цикл.
     *
     * Зачем: в тяжёлом модовом окружении (Sinytra Connector мостит Fabric→NeoForge и
     * дёргает класслоадеры/модули) наш фоновый тред получал тихий interrupt и умирал
     * через ~90с. Смерть heartbeat → сессия гасла reaper'ом → честного игрока выкидывало
     * «Недействительной сессией» при реконнекте. Теперь тред переживает это и один раз
     * репортит причину на backend (diag) — чтобы механизм был виден в логах.
     */
    private static void runResilient(String name, long periodMs, Runnable body) {
        Thread t = new Thread(() -> {
            while (true) {
                try {
                    Thread.sleep(periodMs);
                } catch (InterruptedException e) {
                    // Нас прервали (модлоадер дёрнул тред). НЕ выходим: флаг прерывания уже
                    // сброшен sleep'ом, продолжаем цикл.
                    reportRecovered(name, "interrupted");
                    continue;
                }
                if (guardedIteration(body, (cls, err) -> reportRecovered(name, cls))) {
                    // тело упало с Throwable — уже поглощено и сообщено, цикл живёт дальше
                }
            }
        }, name);
        t.setDaemon(true);
        t.start();
    }

    /**
     * Выполняет одну итерацию тела, поглощая любой Throwable. Возвращает true, если тело
     * упало (для тестов и логики восстановления). Вынесено отдельно, чтобы устойчивость
     * можно было проверить без тредов и сети.
     */
    static boolean guardedIteration(Runnable body, java.util.function.BiConsumer<String, Throwable> onError) {
        try {
            body.run();
            return false;
        } catch (Throwable t) {
            try {
                onError.accept(t.getClass().getSimpleName(), t);
            } catch (Throwable ignored) {
                // отчёт об ошибке сам не должен ронять цикл
            }
            return true;
        }
    }

    // Дедуп diag-репортов: каждый тред сообщает о самовосстановлении один раз.
    private static final java.util.Set<String> recoveredReported =
        java.util.concurrent.ConcurrentHashMap.newKeySet();

    /** Один раз на тред сообщает backend, что фоновый тред пережил interrupt/Throwable. */
    private static void reportRecovered(String name, String cause) {
        if (!recoveredReported.add(name)) {
            return;
        }
        postDiag("thread-recovered", name + ":" + cause);
    }

    /** Лёгкая телеметрия на backend (только лог на сервере, без бизнес-логики). */
    private static void postDiag(String event, String detail) {
        postRead("/api/anticheat/diag",
            "{\"launchToken\":\"" + escape(token) + "\",\"event\":\"" + escape(event)
                + "\",\"detail\":\"" + escape(detail) + "\"}");
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
            List<Rule> parsed = parseRules(body);
            if (!parsed.isEmpty()) {
                // Серверные правила (с match_type) + дефолтный seed для паттернов, которых
                // сервер не прислал: усечённый блэклист не должен ослаблять детект.
                java.util.Set<String> seen = new java.util.HashSet<>();
                List<Rule> merged = new ArrayList<>();
                for (Rule r : parsed) {
                    merged.add(r);
                    seen.add(r.pattern());
                }
                for (String m : DEFAULT_MARKERS) {
                    if (!seen.contains(m)) {
                        merged.add(new Rule(m, "substring", ""));
                    }
                }
                rules = merged;
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

    /** Читает JSON-строку с позиции pos (сразу после открывающей кавычки). Возвращает
     *  декодированное значение; endPos[0] — индекс закрывающей кавычки. */
    private static String readJsonStringAt(String body, int pos, int[] endPos) {
        StringBuilder sb = new StringBuilder();
        int end = pos;
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
        endPos[0] = end;
        return sb.toString();
    }

    /** Извлекает правила {pattern, matchType, hash} из ответа /rules. matchType и hash
     *  каждого объекта идут после его pattern (порядок полей RuleSignature) и до следующего
     *  pattern. Hash-правила приходят с пустым pattern — берём их по непустому hash. */
    private static List<Rule> parseRules(String body) {
        List<Rule> out = new ArrayList<>();
        String patKey = "\"pattern\":\"";
        String mtKey = "\"matchType\":\"";
        String hashKey = "\"hash\":\"";
        int idx = 0;
        int[] end = new int[1];
        while ((idx = body.indexOf(patKey, idx)) >= 0) {
            int patStart = idx + patKey.length();
            String pattern = readJsonStringAt(body, patStart, end).trim().toLowerCase(Locale.ROOT);
            idx = end[0];
            int nextPat = body.indexOf(patKey, idx);

            String matchType = "substring";
            int mtIdx = body.indexOf(mtKey, idx);
            if (mtIdx >= 0 && (nextPat < 0 || mtIdx < nextPat)) {
                String mt = readJsonStringAt(body, mtIdx + mtKey.length(), end).trim().toLowerCase(Locale.ROOT);
                if (!mt.isEmpty()) {
                    matchType = mt;
                }
            }

            String hash = "";
            int hIdx = body.indexOf(hashKey, idx);
            if (hIdx >= 0 && (nextPat < 0 || hIdx < nextPat)) {
                hash = readJsonStringAt(body, hIdx + hashKey.length(), end).trim();
            }

            if (!pattern.isEmpty() || ("hash".equals(matchType) && !hash.isEmpty())) {
                out.add(new Rule(pattern, matchType, hash));
            }
            idx = Math.max(idx, end[0]);
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
            String dotName = className == null ? "" : className.replace('/', '.');
            if (className != null) {
                // Проверяем сырое внутреннее имя (со слэшами) на нелегальные символы.
                if (isIllegalClassName(className)) {
                    reportIllegalName(className, "transform");
                }
                checkAndReport("class", dotName);
            }
            // Hash байткода — даже для className==null (инъекция через defineClass(null,...)).
            checkClassHash(classfileBuffer, dotName);
            // Сигнатурные строки в constant pool (устойчивее hash к рекомпиляции).
            checkClassStrings(classfileBuffer, dotName);
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
