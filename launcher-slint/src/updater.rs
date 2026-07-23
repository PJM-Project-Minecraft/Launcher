//! Самообновление лаунчера: проверка версии на бэкенде, фоновая загрузка
//! бинарника, проверка SHA-256 и подмена себя с перезапуском.
//!
//! Подмена: Linux — атомарный rename поверх работающего бинарника;
//! Windows — rename текущего exe в .old (разрешено) + rename нового на место.

use std::cmp::Ordering;
use std::fs;
use std::io::{Read, Write};
use std::path::{Path, PathBuf};
use std::process::Command;
use std::time::Duration;

use ed25519_dalek::{Signature, VerifyingKey};
use serde::Deserialize;
use sha2::{Digest, Sha256};

/// Версия лаунчера, зашитая при сборке (Cargo.toml).
pub const CURRENT_VERSION: &str = env!("CARGO_PKG_VERSION");

/// Публичный ключ Ed25519 для проверки подписи обновления, вшивается при сборке
/// (`LAUNCHER_UPDATE_PUBKEY` = 64 hex-символа). Задан → подпись ОБЯЗАТЕЛЬНА и
/// проверяется (fail-closed); не задан (dev-сборка) → как раньше, только SHA-256.
fn update_pubkey() -> Option<VerifyingKey> {
    let hex_key = option_env!("LAUNCHER_UPDATE_PUBKEY")?.trim();
    if hex_key.is_empty() {
        return None;
    }
    let bytes = hex::decode(hex_key).ok()?;
    let arr: [u8; 32] = bytes.try_into().ok()?;
    VerifyingKey::from_bytes(&arr).ok()
}

/// Платформа в терминах бэкенда (storage/releases/<version>/<platform>).
pub fn platform() -> &'static str {
    if cfg!(target_os = "windows") {
        "windows-x64"
    } else {
        "linux-x64"
    }
}

#[derive(Debug, Deserialize, Clone, Default)]
#[serde(rename_all = "camelCase")]
pub struct UpdateInfo {
    pub update_available: bool,
    #[serde(default)]
    pub latest_version: String,
    #[serde(default)]
    pub mandatory: bool,
    #[serde(default)]
    pub download_url: String,
    #[serde(default)]
    pub sha256: String,
    /// hex Ed25519-подпись бинарника (пусто — релиз без подписи).
    #[serde(default)]
    pub signature: String,
}

/// Посегментное сравнение версий "X.Y.Z"; отсутствующие и нечисловые
/// сегменты считаются нулями (зеркало CompareVersions на бэкенде).
pub fn compare_versions(a: &str, b: &str) -> Ordering {
    fn parse(version: &str) -> Vec<u64> {
        version
            .split('.')
            .map(|seg| seg.trim().parse::<u64>().unwrap_or(0))
            .collect()
    }
    let (a, b) = (parse(a), parse(b));
    for i in 0..a.len().max(b.len()) {
        let x = a.get(i).copied().unwrap_or(0);
        let y = b.get(i).copied().unwrap_or(0);
        if x != y {
            return x.cmp(&y);
        }
    }
    Ordering::Equal
}

/// true, если версия строго новее текущей. Клиентский guard против навязанного
/// сервером даунгрейда (откат на старую уязвимую версию): решение сервера не слепо.
pub fn is_upgrade(latest_version: &str) -> bool {
    compare_versions(latest_version, CURRENT_VERSION) == Ordering::Greater
}

/// Запрашивает у бэкенда сведения об обновлении для текущей версии и платформы.
pub fn check_update(api_url: &str) -> Result<UpdateInfo, String> {
    let client = crate::hardened_backend_builder()
        .timeout(Duration::from_secs(30))
        .build()
        .map_err(|_| "Не удалось создать HTTP-клиент.".to_string())?;
    let url = format!(
        "{}/api/launcher/update?platform={}&version={}",
        api_url.trim_end_matches('/'),
        platform(),
        CURRENT_VERSION
    );
    let response = client
        .get(url)
        .send()
        .map_err(|_| "Сервер обновлений недоступен.".to_string())?;
    if !response.status().is_success() {
        return Err(format!(
            "Проверка обновлений: HTTP {}",
            response.status().as_u16()
        ));
    }
    response
        .json::<UpdateInfo>()
        .map_err(|_| "Некорректный ответ сервера обновлений.".to_string())
}

fn exe_path() -> Result<PathBuf, String> {
    std::env::current_exe().map_err(|_| "Не удалось определить путь лаунчера.".to_string())
}

/// Временный файл рядом с бинарником: launcher(.exe) -> launcher.update.partial.
/// На Windows with_extension срезает ".exe" (launcher.exe -> launcher.update.partial) — это намеренно; cleanup_leftovers использует те же with_extension, имена согласованы.
fn staging_path(exe: &Path) -> PathBuf {
    exe.with_extension("update.partial")
}

/// Скачивает обновление во временный файл рядом с бинарником и сверяет SHA-256.
/// Возвращает путь к подготовленному файлу. Ошибка создания временного файла
/// означает, что каталог лаунчера не доступен на запись (fallback на ручное
/// обновление).
pub fn download_and_stage(api_url: &str, info: &UpdateInfo) -> Result<PathBuf, String> {
    let exe = exe_path()?;
    let staged = staging_path(&exe);
    let mut out = fs::File::create(&staged).map_err(|_| {
        "Каталог лаунчера недоступен для записи — скачайте новую версию вручную.".to_string()
    })?;

    let client = crate::hardened_backend_builder()
        .connect_timeout(Duration::from_secs(15))
        .tcp_keepalive(Duration::from_secs(20))
        .build()
        .map_err(|_| "Не удалось создать HTTP-клиент.".to_string())?;
    let url = format!("{}{}", api_url.trim_end_matches('/'), info.download_url);
    let mut response = client
        .get(url)
        .send()
        .map_err(|_| "Не удалось скачать обновление.".to_string())?;
    if !response.status().is_success() {
        let _ = fs::remove_file(&staged);
        return Err(format!(
            "Скачивание обновления: HTTP {}",
            response.status().as_u16()
        ));
    }

    let mut buffer = [0u8; 64 * 1024];
    loop {
        let read = match response.read(&mut buffer) {
            Ok(read) => read,
            Err(_) => {
                let _ = fs::remove_file(&staged);
                return Err("Обрыв скачивания обновления.".to_string());
            }
        };
        if read == 0 {
            break;
        }
        if out.write_all(&buffer[..read]).is_err() {
            let _ = fs::remove_file(&staged);
            return Err("Не удалось записать обновление на диск.".to_string());
        }
    }
    drop(out);

    // SHA-256 + подпись. Подпись — корень доверия: SHA приходит тем же каналом, что и
    // файл, и один сам по себе подлинность не доказывает (MITM/скомпром. зеркало
    // подставят и файл, и совпадающий хеш). Ошибка → удаляем недоверенный файл.
    if let Err(e) = verify_staged_file(&staged, info) {
        let _ = fs::remove_file(&staged);
        return Err(e);
    }
    Ok(staged)
}

/// Сверяет подготовленный файл с ожидаемыми SHA-256 и Ed25519-подписью. Читает файл
/// заново — чтобы `apply_and_restart` мог перепроверить прямо перед подменой (TOCTOU:
/// локальный процесс с теми же правами мог подменить `.update.partial` после stage).
fn verify_staged_file(path: &Path, info: &UpdateInfo) -> Result<(), String> {
    let data = fs::read(path).map_err(|_| "Не удалось прочитать обновление.".to_string())?;
    let actual = format!("{:x}", Sha256::digest(&data));
    if !actual.eq_ignore_ascii_case(info.sha256.trim()) {
        return Err("Контрольная сумма обновления не совпала.".to_string());
    }
    verify_signature(&data, &info.signature)
}

/// Проверяет Ed25519-подпись данных вшитым публичным ключом. Ключ вшит → подпись
/// ОБЯЗАТЕЛЬНА (fail-closed: сервер не «снимет» защиту, прислав пустую подпись).
/// Ключа нет (dev-сборка) → подпись не требуется.
fn verify_signature(data: &[u8], sig_hex: &str) -> Result<(), String> {
    let Some(pubkey) = update_pubkey() else {
        return Ok(());
    };
    let sig_bytes =
        hex::decode(sig_hex.trim()).map_err(|_| "Обновление без корректной подписи.".to_string())?;
    let arr: [u8; 64] = sig_bytes
        .try_into()
        .map_err(|_| "Обновление без корректной подписи.".to_string())?;
    pubkey
        .verify_strict(data, &Signature::from_bytes(&arr))
        .map_err(|_| "Подпись обновления недействительна.".to_string())
}

/// Подменяет текущий бинарник подготовленным файлом и перезапускает лаунчер.
/// При успехе не возвращается (process::exit).
pub fn apply_and_restart(staged: &Path, info: &UpdateInfo) -> Result<(), String> {
    let exe = exe_path()?;

    // Перепроверяем SHA+подпись прямо перед подменой (TOCTOU): между stage и apply
    // файл мог быть изменён другим локальным процессом.
    verify_staged_file(staged, info)?;

    #[cfg(unix)]
    {
        use std::os::unix::fs::PermissionsExt;
        fs::set_permissions(staged, fs::Permissions::from_mode(0o755))
            .map_err(|_| "Не удалось выставить права на обновление.".to_string())?;
        // На Linux rename поверх запущенного бинарника атомарен и разрешён.
        fs::rename(staged, &exe)
            .map_err(|_| "Не удалось заменить бинарник лаунчера.".to_string())?;
    }
    #[cfg(windows)]
    {
        // Windows не даёт перезаписать запущенный exe, но даёт переименовать его.
        let old = exe.with_extension("old");
        let _ = fs::remove_file(&old);
        fs::rename(&exe, &old)
            .map_err(|_| "Не удалось переименовать текущий лаунчер.".to_string())?;
        if fs::rename(staged, &exe).is_err() {
            // Откат: возвращаем старый бинарник на место.
            let _ = fs::rename(&old, &exe);
            return Err("Не удалось установить обновление.".to_string());
        }
    }

    Command::new(&exe).spawn().map_err(|_| {
        "Обновление установлено, но перезапуск не удался — запустите лаунчер вручную.".to_string()
    })?;
    std::process::exit(0);
}

/// Удаляет следы прошлых обновлений (вызывается при старте лаунчера).
/// Ошибки игнорируются: .old может ещё держать завершающийся старый процесс.
pub fn cleanup_leftovers() {
    if let Ok(exe) = exe_path() {
        let _ = fs::remove_file(exe.with_extension("old"));
        let _ = fs::remove_file(exe.with_extension("update.partial"));
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn compare_versions_orders_numerically() {
        assert_eq!(compare_versions("1.0.0", "1.0.0"), Ordering::Equal);
        assert_eq!(compare_versions("0.1.0", "0.2.0"), Ordering::Less);
        assert_eq!(compare_versions("0.10.0", "0.9.0"), Ordering::Greater);
        assert_eq!(compare_versions("1.2", "1.2.0"), Ordering::Equal);
        assert_eq!(compare_versions("abc", "0.0.1"), Ordering::Less);
    }

    #[test]
    fn staging_path_is_sibling_of_exe() {
        let staged = staging_path(Path::new("/opt/launcher/launcher-slint"));
        assert_eq!(
            staged,
            PathBuf::from("/opt/launcher/launcher-slint.update.partial")
        );
        let staged_win = staging_path(Path::new("C:/launcher/launcher.exe"));
        assert!(staged_win.to_string_lossy().ends_with("launcher.update.partial"));
    }
}
