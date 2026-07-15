//! Pre-launch handshake с бэкендом: blacklist процессов, init (баны/форс-апдейт),
//! confirm (fallback). Явная модель исхода `InitOutcome` — единственное место, где
//! решается fail-open vs блок запуска.

use serde::Deserialize;

use super::scan;
use crate::{http_client, AppConfig};

/// Сигнатура из блэклиста (`/api/anticheat/blacklist`). match_type — аддитивное поле
/// (serde default): на старом сервере без него десериализация не ломается. Прочие поля
/// JSON (hashHex и т.п.) serde игнорирует — лаунчер матчит процессы только по pattern.
#[derive(Debug, Clone, Deserialize)]
pub struct Signature {
    pub kind: String,
    pub pattern: String,
    #[serde(default)]
    pub severity: i32,
    #[serde(rename = "matchType", default)]
    pub match_type: String,
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
    #[serde(default)]
    challenge: String,
}

/// Исход handshake/init. Allowed/Blocked/UpdateRequired приходят от сервера;
/// Unavailable — сеть/сбой, означает FAIL-OPEN (запуск без enforcement, бэкенд
/// добьёт verified-гейтом на join).
pub enum InitOutcome {
    Allowed {
        token: String,
        nonce: String,
        challenge: String,
    },
    Blocked(String),
    UpdateRequired(String),
    /// 451: пользователь не принял актуальную Политику конфиденциальности.
    PolicyRequired,
    Unavailable,
}

/// Сентинел-ошибка: по ней main показывает экран политики вместо текста.
pub const POLICY_REQUIRED_ERR: &str = "__policy_required__";

/// Тянет блэклист сигнатур (с auth). Err — недоступен (тогда скан пустой).
pub fn fetch_blacklist(config: &AppConfig, token: &str) -> Result<Vec<Signature>, String> {
    let client = http_client()?;
    let url = format!(
        "{}/api/anticheat/blacklist",
        config.api_url().trim_end_matches('/')
    );
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

/// Выполняет handshake/init. Любой сетевой/парсинговый сбой → Unavailable (fail-open).
pub fn init(
    config: &AppConfig,
    token: &str,
    components: &super::hwid::HwidComponents,
    detections: &[scan::Detection],
) -> InitOutcome {
    let Ok(client) = http_client() else {
        return InitOutcome::Unavailable;
    };
    let url = format!(
        "{}/api/anticheat/handshake/init",
        config.api_url().trim_end_matches('/')
    );
    // hwidHash — агрегат (совместимость со старыми банами); hwidComponents — раздельные
    // хеши для fuzzy-матча. Старый сервер hwidComponents игнорирует.
    let body = serde_json::json!({
        "hwidHash": components.aggregate,
        "hwidComponents": components,
        "detections": detections,
    });
    let Ok(response) = client
        .post(url)
        .bearer_auth(token)
        // Серверный форс-апдейт: бэкенд отвечает 426, если версия ниже обязательной.
        .header("X-Launcher-Version", env!("CARGO_PKG_VERSION"))
        .json(&body)
        .send()
    else {
        return InitOutcome::Unavailable;
    };

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
        return InitOutcome::UpdateRequired(message);
    }
    // 451 = не принята Политика конфиденциальности: launch-token не выдан.
    if status.as_u16() == 451 {
        return InitOutcome::PolicyRequired;
    }
    // 403 = заблокирован (allowed:false с причиной); success = разрешён.
    if status.as_u16() == 403 || status.is_success() {
        let Ok(result) = response.json::<InitResult>() else {
            return InitOutcome::Unavailable;
        };
        if !result.allowed {
            let reason = if result.reason.is_empty() {
                "Запуск заблокирован системой защиты.".to_string()
            } else {
                result.reason
            };
            return InitOutcome::Blocked(reason);
        }
        return InitOutcome::Allowed {
            token: result.launch_token,
            nonce: result.nonce,
            challenge: result.challenge,
        };
    }
    InitOutcome::Unavailable
}

/// Подтверждает handshake. Начиная с M3 confirm делает Java-агент внутри JVM,
/// поэтому функция оставлена как fallback/диагностика и в обычном потоке не вызывается.
#[allow(dead_code)]
pub fn confirm(config: &AppConfig, launch_token: &str) -> Result<(), String> {
    if launch_token.is_empty() {
        return Err("Античит не инициализирован (нет launch-token).".to_string());
    }
    let client = http_client()?;
    let url = format!(
        "{}/api/anticheat/handshake/confirm",
        config.api_url().trim_end_matches('/')
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
