/*
 * Захват экрана в нативном агенте (перенесён из процесса лаунчера).
 *
 * Зачем перенесли:
 *   1. Обход. Раньше кадр снимал процесс лаунчера — игрок просто убивал лаунчер после
 *      старта игры, и скриншоты умирали (игра жила на heartbeat Java-агента). Нативка
 *      грузится в саму JVM: убить её = убить игру.
 *   2. Чёрный экран/застывший кадр на Windows. Лаунчер снимал GDI-шным BitBlt с DC
 *      экрана: для игры в эксклюзивном полноэкранном режиме этот DC не содержит кадр
 *      игры → чёрный или последняя composed-картинка рабочего стола (тот самый
 *      «застывший» кадр). Здесь Windows-путь — DXGI Desktop Duplication (снимает вывод
 *      монитора, включая эксклюзивный фуллскрин), GDI остался фолбэком (RDP/VM/старое
 *      железо, где дупликация недоступна).
 *
 * Протокол с Java-агентом (файлы рядом с flag-файлом; Java не умеет снимать экран сам,
 * а нативка не умеет в HTTPS — поэтому кадр носит Java, а подлинность даёт подпись):
 *   <flag>.key      — 64 hex секрета подписи. Пишет ЛАУНЧЕР до старта JVM, нативка
 *                     читает его в Agent_OnLoad и СРАЗУ удаляет — до того, как в JVM
 *                     выполнится хоть один Java-класс. Мод внутри игры секрет не увидит.
 *   <flag>.shot.req — id запроса скриншота (пишет Java, получив его с бэкенда).
 *   <flag>.shot.raw — сырые пиксели RGB24 построчно (пишет нативка).
 *   <flag>.shot.sig — «<hex подписи>\n<ширина> <высота>\n». Пишется ПОСЛЕДНИМ = маркер
 *                     готовности кадра.
 *   <flag>.shot.err — причина неудачи (нативка), Java репортит fail на бэкенд.
 *
 * Подпись: HMAC-SHA256(секрет, "<id>\n<w>\n<h>\n" || сырые RGB). Бэкенд декодирует
 * присланный PNG (lossless!), берёт те же пиксели и сверяет — подменить кадр по дороге
 * (мод в JVM, внешний скрипт, патченный лаунчер) без секрета нельзя.
 */

#include "agent.h"
#include "sha256.h"

#include <stdio.h>
#include <stdlib.h>
#include <string.h>

/* Потолок ширины кадра: 4K-снимок в сыром виде — 25 МБ на диск и столько же в PNG-энкодер
 * внутри игровой JVM. 1080p (самое частое) проходит без ужатия, 4K режется вдвое. */
#define AC_SHOT_MAX_WIDTH 1920
/* Период опроса файла-триггера. Админ ждёт скриншот вживую, поэтому реагируем быстро. */
#define AC_SHOT_POLL_MS 500

static unsigned char g_key[32];
static int g_key_ready = 0;
static char g_req_path[4096];
static char g_raw_path[4096];
static char g_sig_path[4096];
static char g_err_path[4096];

/* ───────────────────────── HMAC-SHA256 (поверх sha256.h) ───────────────────────── */

/* Сообщение разбито на две части, чтобы не склеивать заголовок с мегабайтами пикселей
 * в отдельный буфер. */
static void hmac_sha256(const unsigned char *key, size_t keylen,
                        const unsigned char *m1, size_t l1,
                        const unsigned char *m2, size_t l2,
                        char *out_hex) {
    unsigned char k[64], ipad[64], opad[64], inner[32], outer[32];
    memset(k, 0, sizeof(k));
    if (keylen > 64) {
        ac_sha256_ctx c;
        ac_sha256_init(&c);
        ac_sha256_update(&c, key, keylen);
        ac_sha256_final(&c, k);
    } else {
        memcpy(k, key, keylen);
    }
    for (int i = 0; i < 64; i++) {
        ipad[i] = (unsigned char)(k[i] ^ 0x36);
        opad[i] = (unsigned char)(k[i] ^ 0x5c);
    }
    ac_sha256_ctx c;
    ac_sha256_init(&c);
    ac_sha256_update(&c, ipad, 64);
    if (m1 && l1) {
        ac_sha256_update(&c, m1, l1);
    }
    if (m2 && l2) {
        ac_sha256_update(&c, m2, l2);
    }
    ac_sha256_final(&c, inner);

    ac_sha256_init(&c);
    ac_sha256_update(&c, opad, 64);
    ac_sha256_update(&c, inner, 32);
    ac_sha256_final(&c, outer);

    static const char *hex = "0123456789abcdef";
    for (int i = 0; i < 32; i++) {
        out_hex[i * 2] = hex[(outer[i] >> 4) & 0xf];
        out_hex[i * 2 + 1] = hex[outer[i] & 0xf];
    }
    out_hex[64] = '\0';
}

static int hex_val(char c) {
    if (c >= '0' && c <= '9') return c - '0';
    if (c >= 'a' && c <= 'f') return c - 'a' + 10;
    if (c >= 'A' && c <= 'F') return c - 'A' + 10;
    return -1;
}

/* ───────────────────────── Захват экрана по платформам ───────────────────────── */

/* Возвращает malloc'нутый буфер RGB24 (w*h*3) или NULL. */
static unsigned char *ac_capture_rgb(int *out_w, int *out_h, const char **err);

#ifdef _WIN32

#define COBJMACROS
#include <windows.h>
#include <d3d11.h>
#include <dxgi1_2.h>

/* Конвертирует BGRA-строки (с произвольным row pitch) в плотный RGB24. */
static unsigned char *bgra_to_rgb(const unsigned char *src, int pitch, int w, int h) {
    unsigned char *rgb = (unsigned char *)malloc((size_t)w * h * 3);
    if (!rgb) {
        return NULL;
    }
    for (int y = 0; y < h; y++) {
        const unsigned char *row = src + (size_t)y * pitch;
        unsigned char *dst = rgb + (size_t)y * w * 3;
        for (int x = 0; x < w; x++) {
            dst[x * 3 + 0] = row[x * 4 + 2];
            dst[x * 3 + 1] = row[x * 4 + 1];
            dst[x * 3 + 2] = row[x * 4 + 0];
        }
    }
    return rgb;
}

/* GDI-фолбэк: тот же путь, что был в лаунчере. Работает в окне/безрамочном режиме,
 * на эксклюзивном фуллскрине отдаёт чёрный кадр — потому и оставлен лишь фолбэком.
 * CAPTUREBLT добавлен, чтобы в кадр попадали layered-окна (оверлеи читов). */
static unsigned char *capture_gdi(int *out_w, int *out_h, const char **err) {
    int w = GetSystemMetrics(SM_CXSCREEN);
    int h = GetSystemMetrics(SM_CYSCREEN);
    if (w <= 0 || h <= 0) {
        *err = "screen-metrics";
        return NULL;
    }
    HDC screen = GetDC(NULL);
    if (!screen) {
        *err = "getdc";
        return NULL;
    }
    HDC mem = CreateCompatibleDC(screen);
    HBITMAP bmp = mem ? CreateCompatibleBitmap(screen, w, h) : NULL;
    unsigned char *rgb = NULL;
    if (mem && bmp) {
        HGDIOBJ old = SelectObject(mem, bmp);
        if (BitBlt(mem, 0, 0, w, h, screen, 0, 0, SRCCOPY | CAPTUREBLT)) {
            BITMAPINFO bi;
            memset(&bi, 0, sizeof(bi));
            bi.bmiHeader.biSize = sizeof(BITMAPINFOHEADER);
            bi.bmiHeader.biWidth = w;
            bi.bmiHeader.biHeight = -h; /* top-down */
            bi.bmiHeader.biPlanes = 1;
            bi.bmiHeader.biBitCount = 32;
            bi.bmiHeader.biCompression = BI_RGB;
            unsigned char *bgra = (unsigned char *)malloc((size_t)w * h * 4);
            if (bgra && GetDIBits(mem, bmp, 0, (UINT)h, bgra, &bi, DIB_RGB_COLORS)) {
                rgb = bgra_to_rgb(bgra, w * 4, w, h);
            }
            free(bgra);
        }
        SelectObject(mem, old);
    }
    if (bmp) DeleteObject(bmp);
    if (mem) DeleteDC(mem);
    ReleaseDC(NULL, screen);
    if (!rgb) {
        *err = "gdi";
        return NULL;
    }
    *out_w = w;
    *out_h = h;
    return rgb;
}

/* DXGI Desktop Duplication: снимает то, что реально выводится на монитор, включая
 * эксклюзивный фуллскрин (именно поэтому чёрных/застывших кадров здесь не бывает). */
static unsigned char *capture_dxgi(int *out_w, int *out_h, const char **err) {
    ID3D11Device *dev = NULL;
    ID3D11DeviceContext *ctx = NULL;
    IDXGIDevice *dxdev = NULL;
    IDXGIAdapter *adapter = NULL;
    IDXGIOutput *output = NULL;
    IDXGIOutput1 *output1 = NULL;
    IDXGIOutputDuplication *dup = NULL;
    ID3D11Texture2D *staging = NULL;
    unsigned char *rgb = NULL;
    *err = "dxgi";

    D3D_FEATURE_LEVEL got;
    HRESULT hr = D3D11CreateDevice(NULL, D3D_DRIVER_TYPE_HARDWARE, NULL, 0, NULL, 0,
                                   D3D11_SDK_VERSION, &dev, &got, &ctx);
    if (FAILED(hr) || !dev) {
        *err = "d3d11-device";
        goto done;
    }
    if (FAILED(ID3D11Device_QueryInterface(dev, &IID_IDXGIDevice, (void **)&dxdev)) ||
        FAILED(IDXGIDevice_GetAdapter(dxdev, &adapter)) ||
        FAILED(IDXGIAdapter_EnumOutputs(adapter, 0, &output)) ||
        FAILED(IDXGIOutput_QueryInterface(output, &IID_IDXGIOutput1, (void **)&output1))) {
        *err = "dxgi-output";
        goto done;
    }
    hr = IDXGIOutput1_DuplicateOutput(output1, (IUnknown *)dev, &dup);
    if (FAILED(hr) || !dup) {
        *err = "dxgi-duplicate"; /* напр. RDP-сессия или уже занято другим приложением */
        goto done;
    }

    /* Ждём НОВЫЙ кадр: AcquireNextFrame отдаёт и «пустые» события (только курсор),
     * у которых LastPresentTime == 0 — сохранив такой, получили бы устаревшую картинку. */
    for (int attempt = 0; attempt < 40 && !rgb; attempt++) {
        DXGI_OUTDUPL_FRAME_INFO info;
        IDXGIResource *res = NULL;
        hr = IDXGIOutputDuplication_AcquireNextFrame(dup, 250, &info, &res);
        if (hr == DXGI_ERROR_WAIT_TIMEOUT) {
            continue; /* экран не менялся — ждём дальше */
        }
        if (FAILED(hr) || !res) {
            *err = "dxgi-acquire";
            break;
        }
        if (info.LastPresentTime.QuadPart == 0) {
            IDXGIResource_Release(res);
            IDXGIOutputDuplication_ReleaseFrame(dup);
            continue;
        }
        ID3D11Texture2D *tex = NULL;
        if (SUCCEEDED(IDXGIResource_QueryInterface(res, &IID_ID3D11Texture2D, (void **)&tex)) && tex) {
            D3D11_TEXTURE2D_DESC desc;
            ID3D11Texture2D_GetDesc(tex, &desc);
            desc.Usage = D3D11_USAGE_STAGING;
            desc.BindFlags = 0;
            desc.CPUAccessFlags = D3D11_CPU_ACCESS_READ;
            desc.MiscFlags = 0;
            if (SUCCEEDED(ID3D11Device_CreateTexture2D(dev, &desc, NULL, &staging)) && staging) {
                ID3D11DeviceContext_CopyResource(ctx, (ID3D11Resource *)staging, (ID3D11Resource *)tex);
                D3D11_MAPPED_SUBRESOURCE map;
                if (SUCCEEDED(ID3D11DeviceContext_Map(ctx, (ID3D11Resource *)staging, 0,
                                                      D3D11_MAP_READ, 0, &map))) {
                    rgb = bgra_to_rgb((const unsigned char *)map.pData, (int)map.RowPitch,
                                      (int)desc.Width, (int)desc.Height);
                    if (rgb) {
                        *out_w = (int)desc.Width;
                        *out_h = (int)desc.Height;
                    }
                    ID3D11DeviceContext_Unmap(ctx, (ID3D11Resource *)staging, 0);
                }
                ID3D11Texture2D_Release(staging);
                staging = NULL;
            }
            ID3D11Texture2D_Release(tex);
        }
        IDXGIResource_Release(res);
        IDXGIOutputDuplication_ReleaseFrame(dup);
    }

done:
    if (dup) IDXGIOutputDuplication_Release(dup);
    if (output1) IDXGIOutput1_Release(output1);
    if (output) IDXGIOutput_Release(output);
    if (adapter) IDXGIAdapter_Release(adapter);
    if (dxdev) IDXGIDevice_Release(dxdev);
    if (ctx) ID3D11DeviceContext_Release(ctx);
    if (dev) ID3D11Device_Release(dev);
    return rgb;
}

static unsigned char *ac_capture_rgb(int *out_w, int *out_h, const char **err) {
    unsigned char *rgb = capture_dxgi(out_w, out_h, err);
    if (rgb) {
        return rgb;
    }
    return capture_gdi(out_w, out_h, err);
}

#elif defined(__linux__)

#include <dlfcn.h>

/* Xlib грузим через dlopen: жёсткая линковка сделала бы .so незагружаемым на машине
 * без libX11 — а это уронило бы весь нативный агент, а не только скриншоты. */
typedef struct {
    int width, height;
    int xoffset;
    int format;
    char *data;
    int byte_order;
    int bitmap_unit;
    int bitmap_bit_order;
    int bitmap_pad;
    int depth;
    int bytes_per_line;
    int bits_per_pixel;
    /* дальше в XImage идут маски и функции — нам не нужны, читаем только начало */
} ac_ximage;

static unsigned char *ac_capture_rgb(int *out_w, int *out_h, const char **err) {
    void *lib = dlopen("libX11.so.6", RTLD_LAZY);
    if (!lib) {
        lib = dlopen("libX11.so", RTLD_LAZY);
    }
    if (!lib) {
        *err = "no-libx11";
        return NULL;
    }
    void *(*x_open)(const char *) = (void *(*)(const char *))dlsym(lib, "XOpenDisplay");
    int (*x_close)(void *) = (int (*)(void *))dlsym(lib, "XCloseDisplay");
    int (*x_defscreen)(void *) = (int (*)(void *))dlsym(lib, "XDefaultScreen");
    unsigned long (*x_root)(void *, int) = (unsigned long (*)(void *, int))dlsym(lib, "XRootWindow");
    int (*x_width)(void *, int) = (int (*)(void *, int))dlsym(lib, "XDisplayWidth");
    int (*x_height)(void *, int) = (int (*)(void *, int))dlsym(lib, "XDisplayHeight");
    ac_ximage *(*x_getimage)(void *, unsigned long, int, int, unsigned int, unsigned int,
                             unsigned long, int) =
        (ac_ximage * (*)(void *, unsigned long, int, int, unsigned int, unsigned int,
                         unsigned long, int)) dlsym(lib, "XGetImage");
    int (*x_destroy)(ac_ximage *) = (int (*)(ac_ximage *))dlsym(lib, "XDestroyImage");
    if (!x_open || !x_close || !x_defscreen || !x_root || !x_width || !x_height || !x_getimage) {
        dlclose(lib);
        *err = "libx11-symbols";
        return NULL;
    }

    void *dpy = x_open(NULL);
    if (!dpy) {
        dlclose(lib);
        *err = "no-display"; /* Wayland без XWayland или нет доступа к :0 */
        return NULL;
    }
    int screen = x_defscreen(dpy);
    int w = x_width(dpy, screen);
    int h = x_height(dpy, screen);
    unsigned char *rgb = NULL;
    if (w > 0 && h > 0) {
        /* ZPixmap = 2, AllPlanes = ~0 */
        ac_ximage *img = x_getimage(dpy, x_root(dpy, screen), 0, 0, (unsigned)w, (unsigned)h,
                                    ~0UL, 2);
        if (img && img->data && img->bits_per_pixel >= 24) {
            int bpp = img->bits_per_pixel / 8;
            rgb = (unsigned char *)malloc((size_t)w * h * 3);
            if (rgb) {
                for (int y = 0; y < h; y++) {
                    const unsigned char *row =
                        (const unsigned char *)img->data + (size_t)y * img->bytes_per_line;
                    unsigned char *dst = rgb + (size_t)y * w * 3;
                    for (int x = 0; x < w; x++) {
                        /* X11 depth 24/32 на little-endian отдаёт BGR(X). */
                        dst[x * 3 + 0] = row[x * bpp + 2];
                        dst[x * 3 + 1] = row[x * bpp + 1];
                        dst[x * 3 + 2] = row[x * bpp + 0];
                    }
                }
                *out_w = w;
                *out_h = h;
            }
        }
        if (img && x_destroy) {
            x_destroy(img);
        }
    }
    x_close(dpy);
    dlclose(lib);
    if (!rgb) {
        *err = "x11-getimage";
    }
    return rgb;
}

#else

static unsigned char *ac_capture_rgb(int *out_w, int *out_h, const char **err) {
    (void)out_w; (void)out_h;
    *err = "unsupported-os";
    return NULL;
}

#endif

/* ───────────────────────── Даунскейл + запись результата ───────────────────────── */

/* Box-фильтр целым коэффициентом: качества хватает, а дробное масштабирование тут
 * не нужно. Возвращает новый буфер (или исходный, если ужимать нечего). */
static unsigned char *downscale_rgb(unsigned char *src, int sw, int sh, int *dw, int *dh) {
    *dw = sw;
    *dh = sh;
    if (sw <= AC_SHOT_MAX_WIDTH || sw <= 0 || sh <= 0) {
        return src;
    }
    int factor = (sw + AC_SHOT_MAX_WIDTH - 1) / AC_SHOT_MAX_WIDTH;
    int nw = sw / factor;
    int nh = sh / factor;
    if (nw <= 0 || nh <= 0) {
        return src;
    }
    unsigned char *out = (unsigned char *)malloc((size_t)nw * nh * 3);
    if (!out) {
        return src;
    }
    for (int y = 0; y < nh; y++) {
        for (int x = 0; x < nw; x++) {
            unsigned int sum[3] = {0, 0, 0};
            for (int by = 0; by < factor; by++) {
                const unsigned char *row = src + ((size_t)(y * factor + by) * sw + x * factor) * 3;
                for (int bx = 0; bx < factor; bx++) {
                    sum[0] += row[bx * 3 + 0];
                    sum[1] += row[bx * 3 + 1];
                    sum[2] += row[bx * 3 + 2];
                }
            }
            unsigned int n = (unsigned int)(factor * factor);
            unsigned char *dst = out + ((size_t)y * nw + x) * 3;
            dst[0] = (unsigned char)(sum[0] / n);
            dst[1] = (unsigned char)(sum[1] / n);
            dst[2] = (unsigned char)(sum[2] / n);
        }
    }
    free(src);
    *dw = nw;
    *dh = nh;
    return out;
}

static void write_err(const char *reason) {
    FILE *f = fopen(g_err_path, "w");
    if (f) {
        fprintf(f, "%s\n", reason ? reason : "capture");
        fclose(f);
    }
}

/* Один запрос: снять экран, ужать, записать пиксели и подпись. */
static void handle_request(const char *id) {
    int w = 0, h = 0;
    const char *err = "capture";
    unsigned char *rgb = ac_capture_rgb(&w, &h, &err);
    if (!rgb || w <= 0 || h <= 0) {
        free(rgb);
        write_err(err);
        return;
    }
    rgb = downscale_rgb(rgb, w, h, &w, &h);

    FILE *f = fopen(g_raw_path, "wb");
    if (!f) {
        free(rgb);
        write_err("raw-open");
        return;
    }
    size_t n = (size_t)w * h * 3;
    size_t written = fwrite(rgb, 1, n, f);
    fclose(f);
    if (written != n) {
        free(rgb);
        write_err("raw-write");
        return;
    }

    char header[128];
    int hlen = snprintf(header, sizeof(header), "%s\n%d\n%d\n", id, w, h);
    if (hlen < 0 || (size_t)hlen >= sizeof(header)) {
        free(rgb);
        write_err("bad-id");
        return;
    }
    char sig[65];
    hmac_sha256(g_key, sizeof(g_key), (const unsigned char *)header, (size_t)hlen, rgb, n, sig);
    free(rgb);

    /* .sig пишется последним — Java ждёт именно его как признак готовности кадра.
     * Первая строка — id запроса: без него Java не могла отличить кадр ЭТОГО запроса от
     * кадра предыдущего (устаревший .sig на диске давал ложный провал честному игроку). */
    f = fopen(g_sig_path, "w");
    if (!f) {
        write_err("sig-open");
        return;
    }
    fprintf(f, "%s\n%s\n%d %d\n", id, sig, w, h);
    fclose(f);
}

/* Один тик поллера: есть ли файл-триггер от Java-агента. */
static void shot_tick(void) {
    FILE *f = fopen(g_req_path, "r");
    if (!f) {
        return;
    }
    char id[80];
    char *line = fgets(id, sizeof(id), f);
    fclose(f);
    /* Java пишет "<id>\n". Нет перевода строки — файл ещё дописывается (create+write не
     * атомарны): НЕ удаляем, дочитаем на следующем тике. Раньше запрос в этот момент
     * уничтожался навсегда: Java ждала 20с и репортила ложный провал честному игроку. */
    if (!line || !strchr(id, '\n')) {
        return;
    }

    /* id приходит из JVM — санитизируем: только [A-Za-z0-9-], иначе он попадёт в подпись
     * и в имена/логи как есть. */
    size_t j = 0;
    for (size_t i = 0; id[i] && j < sizeof(id) - 1; i++) {
        char c = id[i];
        int ok = (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || (c >= '0' && c <= '9') || c == '-';
        if (ok) {
            id[j++] = c;
        }
    }
    id[j] = '\0';
    /* Запрос прочитан целиком — триггер убираем в любом случае, иначе мусорный id
     * (поддельный клиент) заставлял бы поллер перечитывать файл вечно. */
    remove(g_req_path);
    if (!id[0]) {
        return;
    }
    remove(g_raw_path);
    remove(g_sig_path);
    remove(g_err_path);
    handle_request(id);
}

#ifdef _WIN32

static DWORD WINAPI shot_thread(LPVOID arg) {
    (void)arg;
    for (;;) {
        shot_tick();
        Sleep(AC_SHOT_POLL_MS);
    }
    return 0;
}

static void start_thread(void) {
    HANDLE t = CreateThread(NULL, 0, shot_thread, NULL, 0, NULL);
    if (t) {
        CloseHandle(t);
    }
}

#elif defined(__linux__)

#include <pthread.h>
#include <unistd.h>

static void *shot_thread(void *arg) {
    (void)arg;
    for (;;) {
        shot_tick();
        usleep(AC_SHOT_POLL_MS * 1000);
    }
    return NULL;
}

static void start_thread(void) {
    pthread_t t;
    if (pthread_create(&t, NULL, shot_thread, NULL) == 0) {
        pthread_detach(t);
    }
}

#else

static void start_thread(void) { }

#endif

int ac_screenshot_init(const char *flag_path) {
    if (!flag_path || !*flag_path) {
        return 0;
    }
    char key_path[4096];
    snprintf(key_path, sizeof(key_path), "%s.key", flag_path);
    snprintf(g_req_path, sizeof(g_req_path), "%s.shot.req", flag_path);
    snprintf(g_raw_path, sizeof(g_raw_path), "%s.shot.raw", flag_path);
    snprintf(g_sig_path, sizeof(g_sig_path), "%s.shot.sig", flag_path);
    snprintf(g_err_path, sizeof(g_err_path), "%s.shot.err", flag_path);
    /* Мусор прошлой сессии: иначе Java подхватит чужой кадр как ответ на новый запрос. */
    remove(g_req_path);
    remove(g_raw_path);
    remove(g_sig_path);
    remove(g_err_path);

    FILE *f = fopen(key_path, "r");
    if (!f) {
        return 0; /* старый лаунчер не кладёт ключ — скриншоты остаются на его стороне */
    }
    char hex[80];
    if (!fgets(hex, sizeof(hex), f)) {
        hex[0] = '\0';
    }
    fclose(f);
    /* Ключ живёт в памяти нативки; на диске он не нужен ни секунды дольше — Java-код
     * в этой JVM ещё не выполнялся, поэтому мод его уже не прочитает. */
    remove(key_path);

    for (int i = 0; i < 32; i++) {
        int hi = hex_val(hex[i * 2]);
        int lo = hex_val(hex[i * 2 + 1]);
        if (hi < 0 || lo < 0) {
            memset(g_key, 0, sizeof(g_key));
            return 0;
        }
        g_key[i] = (unsigned char)((hi << 4) | lo);
    }
    g_key_ready = 1;
    return 1;
}

void ac_screenshot_start(void) {
    if (g_key_ready) {
        start_thread();
    }
}
