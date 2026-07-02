//! Захват экрана игрока для античита: админ запрашивает скриншот через дашборд,
//! бэкенд создаёт pending-запрос по nonce онлайн-сессии. Лаунчер, пока игра
//! запущена, периодически опрашивает `/api/anticheat/screenshot/pending` по
//! launch-token, и если есть запрос — захватывает экран (X11/Win32), кодирует в
//! JPEG и грузит на бэкенд. Best-effort: ошибки не роняют игру.

use std::sync::atomic::AtomicBool;
use std::sync::Arc;
use std::thread;
use std::time::Duration;

use crate::anticheat::poll_until;
use crate::http_client;

/// Интервал опроса pending-запросов скриншота. Короткий — для быстрого отклика
/// на запрос админа, но с запасом под лимит rate-limiter'а бэкенда (10/мин).
const POLL_INTERVAL: Duration = Duration::from_secs(5);

/// Запускает фоновый цикл опроса и выполнения скриншот-запросов. Работает, пока
/// игра запущена (stop не взведён). Гаснет вместе с keepalive на закрытии игры.
/// No-op без launch-token (античит недоступен — fail-open, опрос не идёт).
pub fn spawn_screenshot_loop(
    api_url: &str,
    launch_token: &str,
    stop: Arc<AtomicBool>,
) -> thread::JoinHandle<()> {
    let api_url = api_url.to_string();
    let launch_token = launch_token.to_string();
    thread::spawn(move || run(&api_url, &launch_token, &stop))
}

fn run(api_url: &str, launch_token: &str, stop: &AtomicBool) {
    if launch_token.is_empty() {
        return; // fail-open: без launch-token опрос бессмысленен.
    }
    // Один HTTP-клиент на весь цикл — переиспользуем TCP/TLS-пул между опросами
    // (каждые 5с), иначе каждый опрос платит полный TLS-handshake.
    let client = match http_client() {
        Ok(c) => c,
        Err(_) => return,
    };
    let client = std::sync::Arc::new(client);
    poll_until(stop, POLL_INTERVAL, || {
        poll_and_capture(&client, api_url, launch_token)
    });
}

/// Один цикл: опрашивает pending, при наличии — захватывает экран и грузит JPEG.
fn poll_and_capture(client: &reqwest::blocking::Client, api_url: &str, launch_token: &str) {
    let base = api_url.trim_end_matches('/');
    let pending_url = format!("{}/api/anticheat/screenshot/pending", base);

    let resp = match client
        .get(&pending_url)
        .header("X-Launch-Token", launch_token)
        .send()
    {
        Ok(r) => r,
        Err(_) => return,
    };
    // 204 — нет pending-запроса; тишина.
    if resp.status().as_u16() == 204 {
        return;
    }
    if !resp.status().is_success() {
        return;
    }
    let body: PendingResponse = match resp.json() {
        Ok(b) => b,
        Err(_) => return,
    };
    let id = match body.id.as_deref() {
        Some(id) if !id.is_empty() => id.to_string(),
        _ => return,
    };

    match capture_screen_jpeg() {
        Ok((data, w, h)) => upload_screenshot(client, base, launch_token, &id, &data, w, h),
        Err(e) => fail_screenshot(client, base, launch_token, &id, &e),
    }
}

#[derive(serde::Deserialize)]
struct PendingResponse {
    id: Option<String>,
}

/// Загружает JPEG на бэкенд (base64 в JSON, т.к. launch-token не даёт multipart).
fn upload_screenshot(
    client: &reqwest::blocking::Client,
    base: &str,
    launch_token: &str,
    id: &str,
    data: &[u8],
    width: u32,
    height: u32,
) {
    use base64::Engine;
    let url = format!("{}/api/anticheat/screenshot/{}", base, id);
    let b64 = base64::engine::general_purpose::STANDARD.encode(data);
    let body = serde_json::json!({
        "width": width,
        "height": height,
        "data": b64,
    });
    let _ = client
        .post(&url)
        .header("X-Launch-Token", launch_token)
        .json(&body)
        .send();
}

/// Сообщает бэкенду, что захват не удался — POST /screenshot/:id/fail. Ранний
/// сигнал: запись сразу переходит в failed, не дожидаясь 60с-таймаута reaper'а.
fn fail_screenshot(
    client: &reqwest::blocking::Client,
    base: &str,
    launch_token: &str,
    id: &str,
    reason: &str,
) {
    let url = format!("{}/api/anticheat/screenshot/{}/fail", base, id);
    let body = serde_json::json!({ "reason": reason });
    let _ = client
        .post(&url)
        .header("X-Launch-Token", launch_token)
        .json(&body)
        .send();
}

/// Захватывает основной монитор и кодирует в JPEG (качество 75). Возвращает
/// (байты JPEG, ширина, высота).
fn capture_screen_jpeg() -> Result<(Vec<u8>, u32, u32), String> {
    let (rgba, width, height) = capture_raw()?;
    encode_jpeg(&rgba, width, height)
}

#[cfg(target_os = "linux")]
fn capture_raw() -> Result<(Vec<u8>, u32, u32), String> {
    use x11rb::connection::Connection;
    use x11rb::protocol::xproto::{self, ConnectionExt};

    let (conn, screen_num) =
        x11rb::connect(None).map_err(|e| format!("X11 connect: {}", e))?;
    let screen = &conn.setup().roots[screen_num];
    let geom = conn
        .get_geometry(screen.root)
        .map_err(|e| format!("get_geometry: {}", e))?
        .reply()
        .map_err(|e| format!("geom reply: {}", e))?;
    let (width, height) = (geom.width as u32, geom.height as u32);
    let img = conn
        .get_image(
            xproto::ImageFormat::Z_PIXMAP,
            screen.root,
            0,
            0,
            geom.width,
            geom.height,
            !0u32,
        )
        .map_err(|e| format!("get_image: {}", e))?
        .reply()
        .map_err(|e| format!("image reply: {}", e))?;

    let bpp = (img.data.len() as u32) / (width * height).max(1);
    let bpp = if bpp == 0 { 4 } else { bpp };
    // ZPixmap depth=24 → BGRX (B,G,R,X). Конвертируем в RGBA для image crate.
    let mut rgba = Vec::with_capacity((width * height * 4) as usize);
    for px in img.data.chunks_exact(bpp as usize) {
        if px.len() >= 3 {
            rgba.push(px[2]); // R
            rgba.push(px[1]); // G
            rgba.push(px[0]); // B
            rgba.push(0xFF); // A
        }
    }
    Ok((rgba, width, height))
}

#[cfg(target_os = "windows")]
fn capture_raw() -> Result<(Vec<u8>, u32, u32), String> {
    use windows::Win32::Graphics::Gdi::{
        BitBlt, CreateCompatibleBitmap, CreateCompatibleDC, DeleteDC, DeleteObject,
        GetDC, GetDIBits, ReleaseDC, SelectObject, BITMAPINFO, BITMAPINFOHEADER,
        DIB_RGB_COLORS, SRCCOPY,
    };
    use windows::Win32::UI::WindowsAndMessaging::{GetSystemMetrics, SM_CXSCREEN, SM_CYSCREEN};

    unsafe {
        let width = GetSystemMetrics(SM_CXSCREEN);
        let height = GetSystemMetrics(SM_CYSCREEN);
        if width <= 0 || height <= 0 {
            return Err("не удалось определить размер экрана".to_string());
        }
        let (width, height) = (width as u32, height as u32);

        let hdc_screen = GetDC(None);
        if hdc_screen.is_invalid() {
            return Err("GetDC не удался".to_string());
        }
        let hdc_mem = CreateCompatibleDC(hdc_screen);
        if hdc_mem.is_invalid() {
            ReleaseDC(None, hdc_screen);
            return Err("CreateCompatibleDC не удался".to_string());
        }
        let hbmp = CreateCompatibleBitmap(hdc_screen, width as i32, height as i32);
        if hbmp.is_invalid() {
            DeleteDC(hdc_mem);
            ReleaseDC(None, hdc_screen);
            return Err("CreateCompatibleBitmap не удался".to_string());
        }
        let old = SelectObject(hdc_mem, hbmp);
        let _ = BitBlt(hdc_mem, 0, 0, width as i32, height as i32, hdc_screen, 0, 0, SRCCOPY);

        let mut bi = BITMAPINFO {
            bmiHeader: BITMAPINFOHEADER {
                biSize: std::mem::size_of::<BITMAPINFOHEADER>() as u32,
                biWidth: width as i32,
                biHeight: -(height as i32), // top-down
                biPlanes: 1,
                biBitCount: 32,
                biCompression: 0,
                ..Default::default()
            },
            ..Default::default()
        };
        let mut rgba = vec![0u8; (width * height * 4) as usize];
        let n = GetDIBits(
            hdc_mem,
            hbmp,
            0,
            height as u32,
            Some(rgba.as_mut_ptr() as *mut _),
            &mut bi,
            DIB_RGB_COLORS,
        );
        if n == 0 {
            SelectObject(hdc_mem, old);
            DeleteObject(hbmp);
            DeleteDC(hdc_mem);
            ReleaseDC(None, hdc_screen);
            return Err("GetDIBits не удался".to_string());
        }
        // Win32 DIB — BGRA; конвертируем в RGBA.
        for px in rgba.chunks_exact_mut(4) {
            px.swap(0, 2); // B<->R
        }
        SelectObject(hdc_mem, old);
        DeleteObject(hbmp);
        DeleteDC(hdc_mem);
        ReleaseDC(None, hdc_screen);
        Ok((rgba, width, height))
    }
}

#[cfg(not(any(target_os = "linux", target_os = "windows")))]
fn capture_raw() -> Result<(Vec<u8>, u32, u32), String> {
    Err("захват экрана не поддерживается на этой ОС".to_string())
}

/// Кодирует RGBA-буфер в JPEG (качество 75). Возвращает (байты, ширина, высота).
fn encode_jpeg(rgba: &[u8], width: u32, height: u32) -> Result<(Vec<u8>, u32, u32), String> {
    use image::codecs::jpeg::JpegEncoder;
    use image::ExtendedColorType;
    let mut out = Vec::new();
    let mut enc = JpegEncoder::new_with_quality(&mut out, 75);
    enc.encode(rgba, width, height, ExtendedColorType::Rgba8)
        .map_err(|e| format!("jpeg encode: {}", e))?;
    Ok((out, width, height))
}
