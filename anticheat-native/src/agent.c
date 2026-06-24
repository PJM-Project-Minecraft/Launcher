/*
 * Нативный JVMTI-агент античита (M4). Кроссплатформенный (.so/.dll), подключается
 * к JVM Minecraft через -agentpath. Задачи:
 *   - доказать своё присутствие Java-агенту (system property ac.native) — без него
 *     Java-агент сообщит бэкенду, что нативный слой не загрузился;
 *   - ClassFileLoadHook: инспекция имён загружаемых классов на маркеры читов,
 *     включая bootstrap-классы, недоступные Java-инструментации;
 *   - anti-debug: обнаружение отладчика на старте (ptrace / IsDebuggerPresent).
 *
 * Канал с Java-агентом — flag-файл, путь к которому передаётся в опциях агента:
 * -agentpath:libanticheat.so=<flagfile>. Нативный агент пишет туда present/debug/
 * classhook; Java-агент читает тот же путь (через -Dac.native.flag) и включает в
 * confirm. (system property из Agent_OnLoad в HotSpot не доходит до System.getProperty.)
 *
 * Честно: всё в user-space (нет ring0/драйверов), flag-файл спуфится пропатченным
 * лаунчером. Это поднимает планку, не делает обход невозможным (см. план, M6).
 */

#include <jvmti.h>
#include <string.h>
#include <stdio.h>
#include <stdlib.h>
#include <ctype.h>

#include "agent.h"
#include "sha256.h"

/* Диагностические логи только в отладочной сборке (-DAC_DEBUG). В релизе раскрывающие
 * строки ("[anticheat-native] suspect class: ... (wurst)") в бинарь не попадают —
 * простейшая обфускация удалением: strings/RE не видят детект-логику. */
#ifdef AC_DEBUG
#define AC_LOG(...) do { fprintf(stderr, __VA_ARGS__); fflush(stderr); } while (0)
#else
#define AC_LOG(...) do { } while (0)
#endif

#ifdef _WIN32
#include <windows.h>
#else
#include <sys/types.h>
#include <unistd.h>
#endif

/* Маркеры известных чит-клиентов/модов (нижний регистр). Должны совпадать по духу
 * с SUSPECT_MARKERS в Java-агенте. */
static const char *SUSPECT_MARKERS[] = {
    "wurst", "meteorclient", "baritone", "xray", "killaura",
    "aimbot", "liquidbounce", "impactclient", "sigmaclient", "huzuni"
};
static const int SUSPECT_COUNT = (int)(sizeof(SUSPECT_MARKERS) / sizeof(SUSPECT_MARKERS[0]));

/* Путь к файлу событий (<flagfile>.events): нативный агент дописывает сюда
 * рантайм-детекты, Java-агент их читает и шлёт на бэкенд. */
static char g_events_path[4096] = {0};

/* Имя класса нелегально, если содержит символ, который не даёт настоящий компилятор:
 * ASCII control-символы и пунктуацию. Инжекторы дают классам такие имена, чтобы обходить
 * детект по сигнатурам — само наличие = признак инъекции.
 *
 * ВАЖНО: JVM легально допускает в идентификаторах любые юникод-буквы (не только ASCII).
 * Легитимные моды этим пользуются: напр. Axiom прячет пакет, подменяя `o` на греческий
 * омикрон U+03BF (UTF-8 0xCE 0xBF). Поэтому НЕ-ASCII байты (>= 0x80, продолжения UTF-8)
 * считаем легальными; нелегальны только ASCII вне набора букв/цифр/_/$ и разделителей ./.  */
static int is_illegal_class_name(const char *name) {
    if (!name || !*name) {
        return 0;
    }
    for (const unsigned char *p = (const unsigned char *)name; *p; p++) {
        unsigned char c = *p;
        if (c >= 0x80) {
            continue; /* байт UTF-8 не-ASCII буквы (напр. омикрон Axiom) — легально */
        }
        int ok = (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') ||
                 (c >= '0' && c <= '9') || c == '_' || c == '$' || c == '/' || c == '.';
        if (!ok) {
            return 1;
        }
    }
    return 0;
}

/* Сколько событий уже записано: инъектор определяет СОТНИ классов, нам же нужно
 * лишь несколько образцов как сигнал — остальное только флудит бэкенд. */
static int g_event_count = 0;
#define MAX_EVENTS 25

/* Дописывает событие "<type>\t<name>" в файл для Java-агента. Имя САНИТИЗИРУЕТСЯ:
 * имена инъектированных классов содержат control-символы (вкл. \t и \n), которые
 * иначе ломают строковый протокол. Оставляем только печатные ASCII, прочее → '.'. */
void ac_append_event(const char *type, const char *name) {
    if (!g_events_path[0] || g_event_count >= MAX_EVENTS) {
        return;
    }
    g_event_count++;
    char safe[64];
    size_t j = 0;
    for (const unsigned char *p = (const unsigned char *)name; *p && j < sizeof(safe) - 1; p++) {
        safe[j++] = (*p >= 0x21 && *p <= 0x7e) ? (char)*p : '.';
    }
    safe[j] = '\0';
    FILE *f = fopen(g_events_path, "a");
    if (!f) {
        return;
    }
    fprintf(f, "%s\t%s\n", type, safe);
    fclose(f);
}

static void to_lower_copy(const char *src, char *dst, size_t cap) {
    size_t i = 0;
    for (; src && src[i] && i + 1 < cap; i++) {
        dst[i] = (char)tolower((unsigned char)src[i]);
    }
    dst[i] = '\0';
}

/* Возвращает 1, если под отладкой. */
static int debugger_present(void) {
#ifdef _WIN32
    return IsDebuggerPresent() ? 1 : 0;
#else
    /* Linux: TracerPid в /proc/self/status != 0 означает присутствие трассировщика. */
    FILE *f = fopen("/proc/self/status", "r");
    if (!f) {
        return 0;
    }
    char line[256];
    int traced = 0;
    while (fgets(line, sizeof(line), f)) {
        if (strncmp(line, "TracerPid:", 10) == 0) {
            int pid = atoi(line + 10);
            traced = (pid != 0);
            break;
        }
    }
    fclose(f);
    return traced;
#endif
}

/* Пишет флаги присутствия/состояния в flag-файл для Java-агента. */
static void write_flag_file(const char *path, int debug, int classhook) {
    if (!path || !*path) {
        return;
    }
    FILE *f = fopen(path, "w");
    if (!f) {
        return;
    }
    fprintf(f, "present=1\ndebug=%d\nclasshook=%d\n", debug ? 1 : 0, classhook ? 1 : 0);
    fclose(f);
}

static unsigned read_u2(const unsigned char *p) { return ((unsigned)p[0] << 8) | p[1]; }

/* Извлекает имя класса (this_class) напрямую из байт класс-файла. Нужно потому,
 * что при инъекции через defineClass(null,...) JVMTI передаёт name=NULL — реальное
 * имя есть только в байткоде. Так делает и настоящий анти-инжект: инспектирует байты,
 * а не доверяет переданному имени. Возвращает 1 при успехе. */
static int extract_class_name(const unsigned char *d, jint len, char *out, size_t cap) {
    if (!d || len < 10 || cap < 2) {
        return 0;
    }
    if (!(d[0] == 0xCA && d[1] == 0xFE && d[2] == 0xBA && d[3] == 0xBE)) {
        return 0;
    }
    unsigned cp_count = read_u2(d + 8);
    if (cp_count == 0) {
        return 0;
    }
    unsigned *off = (unsigned *)calloc(cp_count, sizeof(unsigned));
    if (!off) {
        return 0;
    }
    jint pos = 10;
    int ok = 1;
    for (unsigned i = 1; i < cp_count && ok; i++) {
        if (pos >= len) { ok = 0; break; }
        off[i] = (unsigned)pos;
        unsigned char tag = d[pos++];
        switch (tag) {
            case 1: { if (pos + 2 > len) { ok = 0; } else { pos += 2 + (jint)read_u2(d + pos); } } break;
            case 7: case 8: case 16: case 19: case 20: pos += 2; break;
            case 15: pos += 3; break;
            case 3: case 4: case 9: case 10: case 11: case 12: case 17: case 18: pos += 4; break;
            case 5: case 6: pos += 8; i++; break; /* Long/Double занимают 2 слота */
            default: ok = 0; break;
        }
    }
    if (!ok || pos + 4 > len) { free(off); return 0; }
    unsigned this_class = read_u2(d + pos + 2);
    if (this_class == 0 || this_class >= cp_count) { free(off); return 0; }
    unsigned ce = off[this_class];
    if (ce == 0 || ce + 3 > (unsigned)len || d[ce] != 7) { free(off); return 0; }
    unsigned name_idx = read_u2(d + ce + 1);
    if (name_idx == 0 || name_idx >= cp_count) { free(off); return 0; }
    unsigned ue = off[name_idx];
    if (ue == 0 || ue + 3 > (unsigned)len || d[ue] != 1) { free(off); return 0; }
    unsigned ulen = read_u2(d + ue + 1);
    if (ue + 3 + ulen > (unsigned)len) { free(off); return 0; }
    size_t copy = ulen < cap - 1 ? ulen : cap - 1;
    memcpy(out, d + ue + 3, copy);
    out[copy] = '\0';
    free(off);
    return 1;
}

/* Счётчик class-hash событий — отдельный от MAX_EVENTS, чтобы хеши и именные детекты
 * не вытесняли друг друга из общего лимита. */
static int g_hash_count = 0;
#define MAX_HASH_EVENTS 60

/* Эмитит "class-hash\t<sha256>\t<name>" для non-bootstrap класса: Java-агент сверит хеш
 * байткода с блэклистом и поймает чит с обфусцированным именем (его маркеры в имени не
 * палят). Bootstrap-классы (loader==NULL: java.*, jdk.*) пропускаем — читов там нет, а
 * поток на старте огромен; отдельный лимит MAX_HASH_EVENTS защищает от флуда модами. */
static void emit_class_hash(jobject loader, const char *cls,
                            const unsigned char *data, jint len) {
    if (loader == NULL || data == NULL || len <= 0) {
        return;
    }
    if (!g_events_path[0] || g_hash_count >= MAX_HASH_EVENTS) {
        return;
    }
    g_hash_count++;
    char hex[65];
    ac_sha256_hex(data, (size_t)len, hex);
    char safe[256];
    size_t j = 0;
    for (const unsigned char *p = (const unsigned char *)cls; *p && j < sizeof(safe) - 1; p++) {
        safe[j++] = (*p >= 0x21 && *p <= 0x7e) ? (char)*p : '.';
    }
    safe[j] = '\0';
    FILE *f = fopen(g_events_path, "a");
    if (!f) {
        return;
    }
    fprintf(f, "class-hash\t%s\t%s\n", hex, safe);
    fclose(f);
}

/* ClassFileLoadHook: вызывается на каждый загружаемый класс (вкл. bootstrap). */
static void JNICALL on_class_file_load(
        jvmtiEnv *jvmti, JNIEnv *jni, jclass class_being_redefined, jobject loader,
        const char *name, jobject protection_domain, jint class_data_len,
        const unsigned char *class_data, jint *new_class_data_len,
        unsigned char **new_class_data) {
    (void)jvmti; (void)jni; (void)class_being_redefined;
    (void)protection_domain;
    (void)new_class_data_len; (void)new_class_data;

    /* При инъекции через defineClass(null,...) name==NULL — достаём имя из байткода. */
    char namebuf[1024];
    const char *cls = name;
    if (!cls) {
        if (!extract_class_name(class_data, class_data_len, namebuf, sizeof(namebuf))) {
            return;
        }
        cls = namebuf;
    }

    /* Нелегальное имя класса — сильнейший признак инъекции (Naming: Illegal). */
    if (is_illegal_class_name(cls)) {
        AC_LOG("[anticheat-native] illegal class name (inject?): %s\n", cls);
        ac_append_event("illegal-class-name", cls);
        return;
    }

    /* Hash байткода (non-bootstrap): Java сверит с блэклистом — ловит обфусцированные имена. */
    emit_class_hash(loader, cls, class_data, class_data_len);

    char lower[512];
    to_lower_copy(cls, lower, sizeof(lower));
    for (int i = 0; i < SUSPECT_COUNT; i++) {
        if (strstr(lower, SUSPECT_MARKERS[i]) != NULL) {
            AC_LOG("[anticheat-native] suspect class: %s (%s)\n", cls, SUSPECT_MARKERS[i]);
            ac_append_event(SUSPECT_MARKERS[i], cls);
            return;
        }
    }
}

JNIEXPORT jint JNICALL Agent_OnLoad(JavaVM *vm, char *options, void *reserved) {
    (void)reserved;
    const char *flag_path = options; /* путь к flag-файлу из -agentpath:lib=<path> */
    int debug = debugger_present();
    int classhook = 0;

    /* Файл событий рядом с flag-файлом: <flagfile>.events. Обнуляем его при старте,
     * чтобы Java-поллер не перечитал детекты прошлой сессии (иначе ложный kick). */
    if (flag_path && *flag_path) {
        snprintf(g_events_path, sizeof(g_events_path), "%s.events", flag_path);
        FILE *truncf = fopen(g_events_path, "w");
        if (truncf) {
            fclose(truncf);
        }
    }

    jvmtiEnv *jvmti = NULL;
    if ((*vm)->GetEnv(vm, (void **)&jvmti, JVMTI_VERSION_1_2) != JNI_OK || jvmti == NULL) {
        AC_LOG("[anticheat-native] failed to get JVMTI env\n");
        write_flag_file(flag_path, debug, 0);
        return JNI_OK; /* не роняем JVM — enforcement обеспечивает сервер */
    }

    /* ClassFileLoadHook на маркеры читов (вкл. bootstrap-классы). */
    jvmtiCapabilities caps;
    memset(&caps, 0, sizeof(caps));
    caps.can_generate_all_class_hook_events = 1;
    if ((*jvmti)->AddCapabilities(jvmti, &caps) == JVMTI_ERROR_NONE) {
        jvmtiEventCallbacks callbacks;
        memset(&callbacks, 0, sizeof(callbacks));
        callbacks.ClassFileLoadHook = &on_class_file_load;
        (*jvmti)->SetEventCallbacks(jvmti, &callbacks, (jint)sizeof(callbacks));
        (*jvmti)->SetEventNotificationMode(jvmti, JVMTI_ENABLE, JVMTI_EVENT_CLASS_FILE_LOAD_HOOK, NULL);
        classhook = 1;
    }

    /* Канал с Java-агентом: present/debug/classhook → flag-файл. */
    write_flag_file(flag_path, debug, classhook);

    AC_LOG("[anticheat-native] loaded (debug=%d classhook=%d)\n", debug, classhook);

    /* Фоновый guard: поллинг загруженных модулей (анти-инжект DLL/.so) + непрерывный
     * anti-debug. Реализация в guard.c, кроссплатформенно. */
    ac_guard_start();
    return JNI_OK;
}

JNIEXPORT void JNICALL Agent_OnUnload(JavaVM *vm) {
    (void)vm;
}
