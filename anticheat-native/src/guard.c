/*
 * Guard-поток нативного агента: непрерывный анти-инжект и anti-debug ПОСЛЕ старта JVM.
 *
 * Идея (как в GravitGuard, но поллингом, а не хуками ntdll — поллинг кроссплатформенный,
 * не триггерит антивирусы и не требует сторонних библиотек): periodically снимаем список
 * загруженных в процесс модулей и сравниваем с baseline. Новый модуль, не принадлежащий
 * доверенным источникам (каталог JVM / каталог игры / системные пути / allowlist оверлеев),
 * — признак инъекции (DLL/.so чита) → событие module-unknown. Плюс непрерывная проверка
 * отладчика (на старте agent.c проверяет один раз; здесь — каждый тик, ловим late-attach).
 *
 * Сигналы доставляются через общий канал .events (ac_append_event) → Java-поллер → бэкенд.
 * Эвристики (module-unknown, ld-preload, debugger-runtime) маппятся Java-агентом как
 * REPORT-ONLY (см. Agent.java) — обкатка против ложных срабатываний; точные сигналы
 * (нелегальные имена классов, маркеры) остаются kill.
 *
 * Honest: всё в user-space. Это поднимает планку (ловит большинство юзермодных инжектов),
 * но не заменяет ring0. Хуки LdrLoadDll/JNI-attach (GravitGuard-ядро) — отдельный
 * Windows-only follow-up (нужна Windows тест-петля + подпись против AV), см. README.
 */

#include "agent.h"

#include <string.h>
#include <stdio.h>
#include <stdlib.h>
#include <ctype.h>

#define GUARD_PERIOD_SEC 5
#define SEEN_CAP 4096

/* Фрагменты путей легитимных модулей (lowercase): оверлеи, GPU-драйверы, медиатулы.
 * Против ложных module-unknown на чистых машинах с Discord/Steam/OBS/NVIDIA и т.п. */
static const char *ALLOW_FRAGMENTS[] = {
    "steam", "gameoverlay", "discord", "nvidia", "nvopengl", "nvoglv", "nvcuda",
    "amd", "atiumd", "atig", "intel", "ig9icd", "igd",
    "rtss", "rivatuner", "overlay", "obs", "fraps", "afterburner", "bandicam",
    "vulkan", "openal", "lwjgl", "glfw", "jemalloc", "mesa", "dri/", "libgl", "nvml",
    "fontconfig", "freetype", "harfbuzz", "libx11", "libxcb", "libwayland", "pulse",
    "discord_game_sdk", "nahimic", "wallpaper engine",
    /* Нативки легитимных модов, распаковываемые JNI во временные папки (/tmp и т.п.):
     * Axiom (imgui от moulberry), Simple Voice Chat (opus/rnnoise через javacpp),
     * Plasmo Voice (кодеки plasmoverse: opus4j/rnnoise4j/speex4j/lame4j). */
    "imgui", "moulberry", "opus", "rnnoise", "javacpp", "speex", "lame4j"
};
static const int ALLOW_COUNT = (int)(sizeof(ALLOW_FRAGMENTS) / sizeof(ALLOW_FRAGMENTS[0]));

/* Множество уже виденных модулей (FNV-1a их пути). Линейный поиск — модулей немного.
 * Тип long long: на Windows (LLP64) unsigned long 32-бит — усёк бы 64-битную константу. */
static unsigned long long g_seen[SEEN_CAP];
static int g_seen_count = 0;
static int g_baseline_done = 0;
static int g_dbg_reported = 0;

static unsigned long long fnv1a(const char *s) {
    unsigned long long h = 1469598103934665603ULL;
    for (const unsigned char *p = (const unsigned char *)s; *p; p++) {
        h ^= (unsigned long long)*p;
        h *= 1099511628211ULL;
    }
    return h;
}

static int seen_contains(unsigned long long h) {
    for (int i = 0; i < g_seen_count; i++) {
        if (g_seen[i] == h) {
            return 1;
        }
    }
    return 0;
}

static void seen_add(unsigned long long h) {
    if (g_seen_count < SEEN_CAP) {
        g_seen[g_seen_count++] = h;
    }
}

static void guard_to_lower(const char *src, char *dst, size_t cap) {
    size_t i = 0;
    for (; src && src[i] && i + 1 < cap; i++) {
        dst[i] = (char)tolower((unsigned char)src[i]);
    }
    dst[i] = '\0';
}

static int is_allowlisted(const char *lower_path) {
    for (int i = 0; i < ALLOW_COUNT; i++) {
        if (strstr(lower_path, ALLOW_FRAGMENTS[i])) {
            return 1;
        }
    }
    return 0;
}

/* basename для события (без полного пути — он может содержать ник игрока и т.п.). */
static const char *guard_basename(const char *path) {
    const char *b = path;
    for (const char *p = path; *p; p++) {
        if (*p == '/' || *p == '\\') {
            b = p + 1;
        }
    }
    return b;
}

/* ---------- Общая обработка обнаруженного модуля ---------- */

/* Регистрирует модуль; при первом проходе (baseline) только запоминает, далее —
 * новый недоверенный модуль репортит как module-unknown (репорт один раз). */
static void guard_observe_module(const char *path, int trusted) {
    unsigned long long h = fnv1a(path);
    if (seen_contains(h)) {
        return;
    }
    seen_add(h);
    if (!g_baseline_done || trusted) {
        return; /* baseline-набор и доверенные источники не репортим */
    }
    ac_append_event("module-unknown", guard_basename(path));
}

/* ======================================================================== */
#ifdef _WIN32

#include <windows.h>
#include <psapi.h>

typedef LONG(NTAPI *NtQIP_t)(HANDLE, ULONG, PVOID, ULONG, PULONG);

static WCHAR g_win_dir[MAX_PATH] = {0};
static WCHAR g_app_dir[MAX_PATH] = {0};
static WCHAR g_java_dir[MAX_PATH] = {0};

static void wlower(const WCHAR *src, WCHAR *dst, size_t cap) {
    size_t i = 0;
    for (; src && src[i] && i + 1 < cap; i++) {
        WCHAR c = src[i];
        dst[i] = (c >= L'A' && c <= L'Z') ? (WCHAR)(c - L'A' + L'a') : c;
    }
    dst[i] = L'\0';
}

static int wstarts_with(const WCHAR *s, const WCHAR *prefix) {
    if (!prefix[0]) {
        return 0;
    }
    WCHAR ls[MAX_PATH], lp[MAX_PATH];
    wlower(s, ls, MAX_PATH);
    wlower(prefix, lp, MAX_PATH);
    return wcsncmp(ls, lp, wcslen(lp)) == 0;
}

static int win_trusted(const WCHAR *wpath, const char *lower_narrow) {
    if (wstarts_with(wpath, g_win_dir) || wstarts_with(wpath, g_app_dir) ||
        wstarts_with(wpath, g_java_dir)) {
        return 1;
    }
    return is_allowlisted(lower_narrow);
}

static int win_debugger_present(void) {
    if (IsDebuggerPresent()) {
        return 1;
    }
    BOOL remote = FALSE;
    if (CheckRemoteDebuggerPresent(GetCurrentProcess(), &remote) && remote) {
        return 1;
    }
    /* ProcessDebugPort через динамически резолвнутый NtQueryInformationProcess (не хук). */
    HMODULE nt = GetModuleHandleW(L"ntdll.dll");
    if (nt) {
        NtQIP_t fn = (NtQIP_t)GetProcAddress(nt, "NtQueryInformationProcess");
        if (fn) {
            DWORD_PTR port = 0;
            if (fn(GetCurrentProcess(), 7 /*ProcessDebugPort*/, &port, sizeof(port), NULL) == 0 && port != 0) {
                return 1;
            }
        }
    }
    return 0;
}

static void win_capture_trusted_dirs(void) {
    GetWindowsDirectoryW(g_win_dir, MAX_PATH);
    GetCurrentDirectoryW(MAX_PATH, g_app_dir);
    HMODULE jvm = GetModuleHandleW(L"jvm.dll");
    if (jvm) {
        WCHAR p[MAX_PATH];
        if (GetModuleFileNameW(jvm, p, MAX_PATH)) {
            /* Каталог JVM: срезаем "\bin\server\jvm.dll" до корня JRE. */
            WCHAR *lib = wcsstr(p, L"\\bin\\");
            if (lib) {
                *lib = L'\0';
            } else {
                WCHAR *slash = wcsrchr(p, L'\\');
                if (slash) {
                    *slash = L'\0';
                }
            }
            wcsncpy(g_java_dir, p, MAX_PATH - 1);
        }
    }
}

static void win_scan_modules(void) {
    HMODULE mods[1024];
    DWORD needed = 0;
    HANDLE proc = GetCurrentProcess();
    if (!EnumProcessModules(proc, mods, sizeof(mods), &needed)) {
        return;
    }
    int n = (int)(needed / sizeof(HMODULE));
    if (n > 1024) {
        n = 1024;
    }
    for (int i = 0; i < n; i++) {
        WCHAR wpath[MAX_PATH];
        if (!GetModuleFileNameW(mods[i], wpath, MAX_PATH)) {
            continue;
        }
        char narrow[MAX_PATH];
        WideCharToMultiByte(CP_UTF8, 0, wpath, -1, narrow, MAX_PATH, NULL, NULL);
        char lower[MAX_PATH];
        guard_to_lower(narrow, lower, sizeof(lower));
        guard_observe_module(narrow, win_trusted(wpath, lower));
    }
}

static DWORD WINAPI guard_thread(LPVOID arg) {
    (void)arg;
    win_capture_trusted_dirs();
    for (;;) {
        win_scan_modules();
        g_baseline_done = 1;
        if (!g_dbg_reported && win_debugger_present()) {
            g_dbg_reported = 1;
            ac_append_event("debugger-runtime", "win-debugger");
        }
        Sleep(GUARD_PERIOD_SEC * 1000);
    }
    return 0;
}

void ac_guard_start(void) {
    HANDLE t = CreateThread(NULL, 0, guard_thread, NULL, 0, NULL);
    if (t) {
        CloseHandle(t);
    }
}

/* ======================================================================== */
#elif defined(__linux__)

#include <pthread.h>
#include <unistd.h>

static char g_cwd[4096] = {0};
static char g_java_root[4096] = {0};

static int lin_starts(const char *s, const char *prefix) {
    return prefix[0] && strncmp(s, prefix, strlen(prefix)) == 0;
}

static int lin_trusted(const char *path) {
    if (lin_starts(path, g_java_root) || lin_starts(path, g_cwd)) {
        return 1;
    }
    if (lin_starts(path, "/usr/") || lin_starts(path, "/lib/") ||
        lin_starts(path, "/lib64/") || lin_starts(path, "/etc/") ||
        lin_starts(path, "/opt/")) {
        return 1;
    }
    char low[4096];
    guard_to_lower(path, low, sizeof(low));
    return is_allowlisted(low);
}

/* Извлекает путь файла из строки /proc/self/maps (после inode, начинается с '/').
 * Возвращает указатель внутри line или NULL (анонимная/спец-область [heap] и т.п.). */
static char *maps_pathname(char *line) {
    char *slash = strchr(line, '/');
    if (!slash) {
        return NULL;
    }
    /* отрезаем перевод строки */
    char *nl = strchr(slash, '\n');
    if (nl) {
        *nl = '\0';
    }
    return slash;
}

static void lin_capture_java_root(void) {
    FILE *f = fopen("/proc/self/maps", "r");
    if (!f) {
        return;
    }
    char line[8192];
    while (fgets(line, sizeof(line), f)) {
        if (strstr(line, "libjvm.so")) {
            char *path = maps_pathname(line);
            if (path) {
                /* JDK-корень: срезаем "/lib/..." после $JH. */
                char *lib = strstr(path, "/lib/");
                size_t keep = lib ? (size_t)(lib - path) : strlen(path);
                if (keep >= sizeof(g_java_root)) {
                    keep = sizeof(g_java_root) - 1;
                }
                memcpy(g_java_root, path, keep);
                g_java_root[keep] = '\0';
            }
            break;
        }
    }
    fclose(f);
}

static int lin_debugger_present(void) {
    FILE *f = fopen("/proc/self/status", "r");
    if (!f) {
        return 0;
    }
    char line[256];
    int traced = 0;
    while (fgets(line, sizeof(line), f)) {
        if (strncmp(line, "TracerPid:", 10) == 0) {
            traced = (atoi(line + 10) != 0);
            break;
        }
    }
    fclose(f);
    return traced;
}

static void lin_scan_modules(void) {
    FILE *f = fopen("/proc/self/maps", "r");
    if (!f) {
        return;
    }
    char line[8192];
    while (fgets(line, sizeof(line), f)) {
        char *path = maps_pathname(line);
        if (!path || path[0] != '/') {
            continue;
        }
        /* Только разделяемые объекты (.so / .so.N) — не data-файлы (.jar, шрифты).
         * Несколько сегментов одной библиотеки схлопываются дедупом по хешу пути. */
        if (!strstr(path, ".so")) {
            continue;
        }
        guard_observe_module(path, lin_trusted(path));
    }
    fclose(f);
}

/* LD_PRELOAD / LD_AUDIT — классические векторы юзермод-инъекции в Linux. Репортим раз. */
static void lin_check_preload(void) {
    const char *vars[] = {"LD_PRELOAD", "LD_AUDIT"};
    for (int i = 0; i < 2; i++) {
        const char *v = getenv(vars[i]);
        if (v && *v) {
            ac_append_event("ld-preload", vars[i]);
        }
    }
}

static void *guard_thread(void *arg) {
    (void)arg;
    if (getcwd(g_cwd, sizeof(g_cwd)) == NULL) {
        g_cwd[0] = '\0';
    }
    lin_capture_java_root();
    lin_check_preload();
    for (;;) {
        lin_scan_modules();
        g_baseline_done = 1;
        if (!g_dbg_reported && lin_debugger_present()) {
            g_dbg_reported = 1;
            ac_append_event("debugger-runtime", "tracerpid");
        }
        sleep(GUARD_PERIOD_SEC);
    }
    return NULL;
}

void ac_guard_start(void) {
    pthread_t t;
    if (pthread_create(&t, NULL, guard_thread, NULL) == 0) {
        pthread_detach(t);
    }
}

/* ======================================================================== */
#else

void ac_guard_start(void) { /* прочие ОС — no-op */ }

#endif
