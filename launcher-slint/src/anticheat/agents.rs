//! Доставка артефактов античита в служебную папку данных (через artifacts::ensure
//! с SHA-сверкой): Java-агент (agent.jar) и нативная JVMTI-библиотека по текущей ОС.
//! Err — подмена (блок запуска); Ok(None) — недоступно (fail-open оффлайн).

use std::path::PathBuf;

use crate::AppConfig;

// Имя нативной JVMTI-библиотеки и токен ОС для эндпоинта раздачи. None — ОС без
// собранной нативной части (агент тогда не инжектится, его отсутствие зафиксирует
// Java-агент как детект).
fn native_target() -> Option<(&'static str, &'static str)> {
    if cfg!(target_os = "linux") {
        Some(("linux", "libanticheat.so"))
    } else if cfg!(target_os = "windows") {
        Some(("windows", "anticheat.dll"))
    } else {
        None
    }
}

/// Гарантирует наличие agent.jar античита (SHA-256 из манифеста). Качает свежий jar,
/// при ошибке — кэш. Err — подмена (блок); Ok(None) — недоступно (fail-open).
pub fn ensure_agent(
    config: &AppConfig,
    expected_sha: Option<&str>,
) -> Result<Option<PathBuf>, String> {
    let Some(dir) = crate::project_dirs().ok().map(|d| d.data_dir().to_path_buf()) else {
        return Ok(None);
    };
    let path = dir.join("anticheat-agent.jar");
    let url = format!(
        "{}/api/anticheat/agent.jar",
        config.api_url.trim_end_matches('/')
    );
    let client = crate::download_client()?;
    crate::artifacts::ensure(&client, &url, &path, &dir, expected_sha).map_err(|e| e.message())
}

/// Гарантирует наличие нативной библиотеки античита по текущей ОС (SHA-сверка).
/// Err — подмена (блок); Ok(None) — нет нативной части для ОС или недоступна (fail-open).
pub fn ensure_native(
    config: &AppConfig,
    expected_sha: Option<&str>,
) -> Result<Option<PathBuf>, String> {
    let Some((os_token, file_name)) = native_target() else {
        return Ok(None);
    };
    let Some(dir) = crate::project_dirs().ok().map(|d| d.data_dir().to_path_buf()) else {
        return Ok(None);
    };
    let path = dir.join(file_name);
    let url = format!(
        "{}/api/anticheat/native/{}",
        config.api_url.trim_end_matches('/'),
        os_token
    );
    let client = crate::download_client()?;
    crate::artifacts::ensure(&client, &url, &path, &dir, expected_sha).map_err(|e| e.message())
}
