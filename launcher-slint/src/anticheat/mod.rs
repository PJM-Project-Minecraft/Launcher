//! Лаунчер-сторона античита (M1): сбор HWID, скан процессов против блэклиста и
//! pre-launch handshake с бэкендом. Бэкенд проверяет баны и при необходимости
//! блокирует запуск (Allowed=false) — тогда guard возвращает Err и игра не стартует.
//!
//! Инжект агентов и привязка launch-token к игровой сессии добавятся в M2–M4.

mod hwid;
mod scan;

use serde::Deserialize;

use crate::{http_client, AppConfig};

/// Сигнатура из блэклиста (/api/anticheat/blacklist).
#[derive(Debug, Clone, Deserialize)]
pub struct Signature {
    pub kind: String,
    pub pattern: String,
    #[serde(default)]
    pub severity: i32,
}

#[derive(Debug, Deserialize)]
#[serde(rename_all = "camelCase")]
struct InitResult {
    allowed: bool,
    #[serde(default)]
    reason: String,
    #[serde(default)]
    launch_token: String,
    #[serde(default)]
    nonce: String,
}

/// Результат успешного pre-launch handshake. nonce связывает игровую сессию с
/// launch-token; launch_token предъявляется при confirm (M2: стаб-агент — сам
/// лаунчер; M3+: Java/нативный агент в JVM).
#[derive(Debug, Clone)]
pub struct GuardOk {
    pub launch_token: String,
    pub nonce: String,
}

/// Ошибки handshake/init, различающие форс-апдейт и сетевые сбои:
/// UpdateRequired блокирует запуск, Network — fail-open (M1).
enum InitError {
    UpdateRequired(String),
    // Полезная нагрузка — диагностика; в M1 не читается (fail-open игнорирует
    // причину), но строку несём для будущего логирования и map_err от http_client.
    Network(#[allow(dead_code)] String),
}

/// Выполняет pre-launch проверки. Возвращает Err с причиной, если запуск заблокирован
/// (бан HWID/аккаунта). Сетевые/прочие ошибки в M1 не блокируют запуск (fail-open),
/// чтобы недоступность бэкенда не ломала игру — enforcement приходит в M2.
pub fn pre_launch_guard(config: &AppConfig, token: &str) -> Result<GuardOk, String> {
    let blacklist = fetch_blacklist(config, token).unwrap_or_default();
    let detections = scan::scan_processes(&blacklist);
    let hwid_hash = hwid::collect_hwid_hash();

    match init_handshake(config, token, &hwid_hash, &detections) {
        Ok(result) => {
            if !result.allowed {
                let reason = if result.reason.is_empty() {
                    "Запуск заблокирован системой защиты.".to_string()
                } else {
                    result.reason
                };
                return Err(reason);
            }
            Ok(GuardOk {
                launch_token: result.launch_token,
                nonce: result.nonce,
            })
        }
        // Форс-апдейт (426): запуск блокируется до обновления лаунчера.
        Err(InitError::UpdateRequired(message)) => Err(message),
        // fail-open в M1: при сетевой ошибке не блокируем игрока.
        Err(InitError::Network(_)) => Ok(GuardOk {
            launch_token: String::new(),
            nonce: String::new(),
        }),
    }
}

/// Подтверждает античит-handshake. Начиная с M3 confirm делает Java-агент внутри
/// JVM, поэтому функция оставлена как fallback/диагностика и в обычном потоке не
/// вызывается.
#[allow(dead_code)]
pub fn confirm(config: &AppConfig, launch_token: &str) -> Result<(), String> {
    if launch_token.is_empty() {
        return Err("Античит не инициализирован (нет launch-token).".to_string());
    }
    let client = http_client()?;
    let url = format!(
        "{}/api/anticheat/handshake/confirm",
        config.api_url.trim_end_matches('/')
    );
    let response = client
        .post(url)
        .json(&serde_json::json!({ "launchToken": launch_token }))
        .send()
        .map_err(|_| "Не удалось подтвердить защиту.".to_string())?;
    if !response.status().is_success() {
        return Err("Сервер отклонил подтверждение защиты.".to_string());
    }
    Ok(())
}

fn fetch_blacklist(config: &AppConfig, token: &str) -> Result<Vec<Signature>, String> {
    let client = http_client()?;
    let url = format!("{}/api/anticheat/blacklist", config.api_url.trim_end_matches('/'));
    let response = client
        .get(url)
        .bearer_auth(token)
        .send()
        .map_err(|_| "blacklist fetch failed".to_string())?;
    if !response.status().is_success() {
        return Err("blacklist http error".to_string());
    }
    response
        .json::<Vec<Signature>>()
        .map_err(|_| "blacklist parse failed".to_string())
}

fn init_handshake(
    config: &AppConfig,
    token: &str,
    hwid_hash: &str,
    detections: &[scan::Detection],
) -> Result<InitResult, InitError> {
    let client = http_client().map_err(InitError::Network)?;
    let url = format!(
        "{}/api/anticheat/handshake/init",
        config.api_url.trim_end_matches('/')
    );
    let body = serde_json::json!({
        "hwidHash": hwid_hash,
        "detections": detections,
    });
    let response = client
        .post(url)
        .bearer_auth(token)
        // Серверный форс-апдейт: бэкенд отвечает 426, если версия ниже
        // минимальной обязательной.
        .header("X-Launcher-Version", env!("CARGO_PKG_VERSION"))
        .json(&body)
        .send()
        .map_err(|_| InitError::Network("init request failed".to_string()))?;

    let status = response.status();
    // 426 = требуется обновление лаунчера: блокируем запуск с сообщением сервера.
    if status.as_u16() == 426 {
        let message = response
            .json::<serde_json::Value>()
            .ok()
            .and_then(|value| {
                value
                    .get("message")
                    .and_then(|m| m.as_str())
                    .map(String::from)
            })
            .unwrap_or_else(|| "Требуется обновление лаунчера.".to_string());
        return Err(InitError::UpdateRequired(message));
    }
    // 403 = запуск заблокирован: тело содержит причину (allowed:false).
    if status.as_u16() == 403 || status.is_success() {
        return response
            .json::<InitResult>()
            .map_err(|_| InitError::Network("init parse failed".to_string()));
    }
    Err(InitError::Network(format!("init http {}", status.as_u16())))
}
