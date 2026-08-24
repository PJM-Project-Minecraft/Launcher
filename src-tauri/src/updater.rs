//! Самообновление лаунчера: проверка версии на бэкенде, фоновая загрузка
//! бинарника, проверка SHA-256 и подмена себя с перезапуском.
//!
//! Подмена: Linux — атомарный rename поверх работающего бинарника;
//! Windows — rename текущего exe в .old (разрешено) + rename нового на место.

use std::cmp::Ordering;
use std::fs;
use std::path::{Path, PathBuf};
use std::process::Command;
use std::time::Duration;

use ed25519_dalek::{Signature, VerifyingKey};
use sha2::{Digest, Sha256};

/// Версия лаунчера, зашитая при сборке (Cargo.toml).
pub const CURRENT_VERSION: &str = env!("CARGO_PKG_VERSION");

/// Самодекларация версии в теле бинарника. Подпись Ed25519 покрывает только байты
/// файла и НЕ говорит, какой это релиз, — поэтому подписанный старый бинарник можно
/// было выдать за новую версию (реплей: канал обновлений полу-доверенный — зеркало
/// терминирует TLS само). Маркер связывает подпись с версией: скачанный файл обязан
/// содержать маркер ЗАЯВЛЕННОЙ версии, а его туда пишет только сборка этой версии.
///
/// Строка реально уходит в заголовок запроса (см. check_update) — так она гарантированно
/// попадает в .rodata и не выпиливается оптимизатором.
pub const VERSION_MARKER: &str = concat!("PMLVER=", env!("CARGO_PKG_VERSION"), ";");

/// Публичный ключ Ed25519 для проверки подписи обновления, вшивается при сборке
/// (`LAUNCHER_UPDATE_PUBKEY` = 64 hex-символа). Задан → подпись ОБЯЗАТЕЛЬНА и
/// проверяется (fail-closed); не задан (dev-сборка) → как раньше, только SHA-256.
fn update_pubkey() -> Result<Option<VerifyingKey>, String> {
    let hex_key = option_env!("LAUNCHER_UPDATE_PUBKEY")
        .map(str::trim)
        .unwrap_or_default();
    if hex_key.is_empty() {
        if cfg!(debug_assertions) {
            return Ok(None);
        }
        return Err("Release-сборка не содержит ключ подписи обновлений.".to_string());
    }
    let bytes = hex::decode(hex_key)
        .map_err(|_| "В сборке повреждён ключ подписи обновлений.".to_string())?;
    let arr: [u8; 32] = bytes
        .try_into()
        .map_err(|_| "В сборке повреждён ключ подписи обновлений.".to_string())?;
    VerifyingKey::from_bytes(&arr)
        .map(Some)
        .map_err(|_| "В сборке повреждён ключ подписи обновлений.".to_string())
}

/// Платформа в терминах бэкенда (storage/releases/<version>/<platform>).
pub fn platform() -> &'static str {
    if cfg!(target_os = "windows") {
        "windows-x64"
    } else {
        "linux-x64"
    }
}

#[derive(Debug, Clone, Default, serde::Deserialize)]
#[serde(rename_all = "camelCase")]
pub struct UpdateInfo {
    pub update_available: bool,
    #[serde(default)]
    pub latest_version: String,
    #[serde(default)]
    pub mandatory: bool,
    #[serde(default)]
    pub sha256: String,
    /// hex Ed25519-подпись бинарника (пусто — релиз без подписи).
    #[serde(default)]
    pub signature: String,
    pub release_id: String,
    pub changelog: String,
    pub artifact: crate::delivery::ReleaseFile,
    #[serde(default)]
    pub delivery_base_url: String,
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
    let mut headers = reqwest::header::HeaderMap::new();
    headers.insert(
        "x-launcher-build-marker",
        reqwest::header::HeaderValue::from_static(VERSION_MARKER),
    );
    let client = crate::hardened_backend_builder()
        .timeout(Duration::from_secs(30))
        .default_headers(headers)
        .build()
        .map_err(|_| "Не удалось создать HTTP-клиент.".to_string())?;
    let Some(manifest) =
        crate::delivery::fetch_launcher_manifest(&client, api_url, platform(), CURRENT_VERSION)?
    else {
        return Ok(UpdateInfo {
            latest_version: CURRENT_VERSION.to_string(),
            ..Default::default()
        });
    };
    Ok(UpdateInfo {
        update_available: true,
        latest_version: manifest.version,
        mandatory: manifest.mandatory,
        sha256: manifest.artifact.sha256.clone(),
        signature: manifest.artifact_signature,
        release_id: manifest.release_id,
        changelog: manifest.changelog,
        artifact: manifest.artifact,
        delivery_base_url: manifest.delivery_base_url,
    })
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
pub fn download_and_stage(
    api_url: &str,
    info: &UpdateInfo,
    progress: &(dyn Fn(usize, usize) + Sync),
) -> Result<PathBuf, String> {
    let exe = exe_path()?;
    let staged = staging_path(&exe);
    let client = crate::backend_download_client()?;
    let cache_root = exe
        .parent()
        .unwrap_or_else(|| Path::new("."))
        .join(".launcher-chunks-v2");
    crate::delivery::reconstruct_file(
        &client,
        api_url,
        &info.delivery_base_url,
        None,
        &crate::delivery::Scope::Launcher {
            release_id: info.release_id.clone(),
        },
        &cache_root,
        &info.artifact,
        &staged,
        progress,
    )?;

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
    verify_signature(&data, &info.signature)?;
    verify_declares_version(&data, &info.latest_version)
}

/// Требует, чтобы скачанный бинарник сам объявлял ЗАЯВЛЕННУЮ версию и был собран под
/// нашу платформу. Закрывает реплей: подпись старого релиза (или релиза для другой ОС)
/// валидна по байтам, но маркер версии в нём другой, а формат исполняемого файла чужой.
fn verify_declares_version(data: &[u8], version: &str) -> Result<(), String> {
    let want = format!("{}{};", "PMLVER=", version.trim());
    let bytes = want.as_bytes();
    if data.len() < bytes.len() || !data.windows(bytes.len()).any(|w| w == bytes) {
        return Err("Обновление не объявляет заявленную версию — отклонено.".to_string());
    }
    let native_format = if cfg!(target_os = "windows") {
        data.starts_with(b"MZ")
    } else {
        data.starts_with(b"\x7fELF")
    };
    if !native_format {
        return Err("Обновление собрано для другой платформы — отклонено.".to_string());
    }
    Ok(())
}

/// Проверяет Ed25519-подпись данных вшитым публичным ключом. Ключ вшит → подпись
/// ОБЯЗАТЕЛЬНА (fail-closed: сервер не «снимет» защиту, прислав пустую подпись).
/// Ключа нет (dev-сборка) → подпись не требуется.
fn verify_signature(data: &[u8], sig_hex: &str) -> Result<(), String> {
    let Some(pubkey) = update_pubkey()? else {
        return Ok(());
    };
    let sig_bytes = hex::decode(sig_hex.trim())
        .map_err(|_| "Обновление без корректной подписи.".to_string())?;
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
    use std::io::{Read, Write};
    use std::net::TcpListener;

    #[test]
    fn compare_versions_orders_numerically() {
        assert_eq!(compare_versions("1.0.0", "1.0.0"), Ordering::Equal);
        assert_eq!(compare_versions("0.1.0", "0.2.0"), Ordering::Less);
        assert_eq!(compare_versions("0.10.0", "0.9.0"), Ordering::Greater);
        assert_eq!(compare_versions("1.2", "1.2.0"), Ordering::Equal);
        assert_eq!(compare_versions("abc", "0.0.1"), Ordering::Less);
    }

    #[test]
    fn update_check_keeps_runtime_delivery_base_outside_signed_descriptor() {
        let platform = platform();
        let body = serde_json::json!({
            "schemaVersion": 2,
            "kind": "launcher",
            "releaseId": "11111111-1111-1111-1111-111111111111",
            "version": "99.0.0",
            "platform": platform,
            "changelog": "CDN",
            "artifactSignature": "",
            "artifact": {
                "path": "launcher",
                "size": 1,
                "sha256": "a".repeat(64),
                "executable": true,
                "chunks": [{"sha256": "b".repeat(64), "size": 1}]
            }
        })
        .to_string();
        let digest = format!("{:x}", Sha256::digest(body.as_bytes()));
        let listener = TcpListener::bind("127.0.0.1:0").unwrap();
        let address = listener.local_addr().unwrap();
        let server = std::thread::spawn(move || {
            let (mut stream, _) = listener.accept().unwrap();
            let mut request = [0_u8; 4096];
            let _ = stream.read(&mut request).unwrap();
            write!(
                stream,
                "HTTP/1.1 200 OK\r\nContent-Type: application/json\r\nContent-Length: {}\r\nX-Manifest-SHA256: {}\r\nX-Manifest-Signature: \r\nX-Update-Mandatory: false\r\nX-Delivery-Base-URL: https://cdn.example.com\r\nConnection: close\r\n\r\n{}",
                body.len(),
                digest,
                body
            )
            .unwrap();
        });

        let info = check_update(&format!("http://{address}")).unwrap();
        server.join().unwrap();
        assert_eq!(info.delivery_base_url, "https://cdn.example.com");
    }

    // Реплей подписанного старого бинарника под видом новой версии должен отваливаться:
    // маркер версии внутри файла не совпадёт с заявленным.
    #[test]
    fn version_marker_binds_binary_to_declared_version() {
        let elf_magic: &[u8] = if cfg!(target_os = "windows") {
            b"MZ"
        } else {
            b"\x7fELF"
        };
        let mut old_build = elf_magic.to_vec();
        old_build.extend_from_slice(b"....PMLVER=0.4.0;....");

        assert!(verify_declares_version(&old_build, "0.4.0").is_ok());
        assert!(
            verify_declares_version(&old_build, "9.9.9").is_err(),
            "бинарник версии 0.4.0 нельзя выдавать за 9.9.9"
        );
        assert!(
            verify_declares_version(&old_build, "").is_err(),
            "пустая версия — не повод пропускать файл"
        );
        // Сборка под другую ОС: маркер тот же, формат исполняемого файла чужой.
        let mut foreign = b"\xCA\xFE\xBA\xBE".to_vec();
        foreign.extend_from_slice(b"PMLVER=0.4.0;");
        assert!(verify_declares_version(&foreign, "0.4.0").is_err());
    }

    // Свой же бинарник обязан содержать маркер собственной версии — иначе проверка
    // была бы бесполезной (в файлах релиза маркера не окажется).
    #[test]
    fn current_binary_declares_its_version() {
        assert_eq!(VERSION_MARKER, format!("PMLVER={};", CURRENT_VERSION));
        let exe = std::env::current_exe().expect("current_exe");
        let data = std::fs::read(exe).expect("read self");
        let marker = format!("PMLVER={};", CURRENT_VERSION);
        assert!(
            data.windows(marker.len()).any(|w| w == marker.as_bytes()),
            "маркер версии должен попадать в бинарник"
        );
    }

    #[test]
    fn staging_path_is_sibling_of_exe() {
        let staged = staging_path(Path::new("/opt/launcher/project-minecraft-launcher"));
        assert_eq!(
            staged,
            PathBuf::from("/opt/launcher/project-minecraft-launcher.update.partial")
        );
        let staged_win = staging_path(Path::new("C:/launcher/launcher.exe"));
        assert!(staged_win
            .to_string_lossy()
            .ends_with("launcher.update.partial"));
    }
}
