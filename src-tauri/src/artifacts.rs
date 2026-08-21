//! Верифицированный загрузчик артефактов: скачивание с SHA-256-сверкой, до 2 попыток
//! при подмене, откат на кэш. Общий для agent.jar / нативной библиотеки /
//! authlib-injector — НЕ зависит от модуля anticheat.

use std::fs;
use std::path::{Path, PathBuf};

/// Несовпадение SHA-256 — подмена артефакта (MITM или локально) → запуск блокируется.
#[derive(Debug)]
pub enum IntegrityError {
    Tampered(String),
}

impl IntegrityError {
    /// Текст для показа игроку (запуск заблокирован).
    pub fn message(&self) -> String {
        match self {
            IntegrityError::Tampered(name) => format!(
                "Контроль целостности не пройден: {} подменён — запуск заблокирован.",
                name
            ),
        }
    }
}

/// Пустую строку SHA трактуем как «ожидаемого хэша нет» (файла нет на бэкенде).
pub fn sha_opt(s: &str) -> Option<&str> {
    if s.is_empty() {
        None
    } else {
        Some(s)
    }
}

/// Сверяет SHA-256 файла с ожидаемым. Ok(()) — совпал или сверка не требуется;
/// Err — не совпал (подмена); ошибку чтения при наличии sha тоже трактуем как подмену.
pub fn verify_sha(path: &Path, expected: Option<&str>) -> Result<(), IntegrityError> {
    let Some(sha) = expected else {
        return Ok(());
    };
    let name = path
        .file_name()
        .and_then(|n| n.to_str())
        .unwrap_or("файл агента")
        .to_string();
    match crate::hash_file(path) {
        Ok(h) if h.eq_ignore_ascii_case(sha) => Ok(()),
        _ => Err(IntegrityError::Tampered(name)),
    }
}

// Скачивает url → path (атомарно через .part) и, если задан expected, сверяет хэш.
// Ok(true) — файл на месте и валиден; Ok(false) — скачать не удалось (сеть/HTTP);
// Err — файл скачан, но SHA не совпал (подмена).
fn download_and_verify(
    client: &crate::DownloadClient,
    url: &str,
    path: &Path,
    dir: &Path,
    expected: Option<&str>,
) -> Result<bool, IntegrityError> {
    if fs::create_dir_all(dir).is_err() {
        return Ok(false);
    }
    let Ok(response) = client.get(url, None, None) else {
        return Ok(false);
    };
    if !response.status().is_success() {
        return Ok(false);
    }
    let mut bytes = Vec::new();
    if response
        .consume(|chunk| {
            bytes.extend_from_slice(chunk);
            Ok(())
        })
        .is_err()
    {
        return Ok(false);
    }
    let tmp = path.with_extension("part");
    if fs::write(&tmp, &bytes).is_err() {
        return Ok(false);
    }
    install_verified(&tmp, path, expected)
}

// Ставит скачанный файл на место ТОЛЬКО после сверки хэша: иначе недоверенный ответ
// (подменённое зеркало, битая раздача) затирал уже проверенный кэш, и следующий запуск
// блокировался навсегда — даже оффлайн-откат брать было уже нечего.
fn install_verified(
    tmp: &Path,
    path: &Path,
    expected: Option<&str>,
) -> Result<bool, IntegrityError> {
    if verify_sha(tmp, expected).is_err() {
        let _ = fs::remove_file(tmp);
        let name = path
            .file_name()
            .and_then(|n| n.to_str())
            .unwrap_or("файл агента")
            .to_string();
        return Err(IntegrityError::Tampered(name));
    }
    if fs::rename(tmp, path).is_err() {
        let _ = fs::remove_file(tmp);
        return Ok(false);
    }
    Ok(true)
}

/// Скачивает артефакт с SHA-сверкой (до 2 попыток при подмене/битой загрузке), с
/// откатом на кэш. Ok(Some) — готов и валиден; Ok(None) — недоступен (оффлайн без
/// кэша, fail-open); Err — подмена (блок запуска).
pub fn ensure(
    client: &crate::DownloadClient,
    url: &str,
    path: &Path,
    dir: &Path,
    expected: Option<&str>,
) -> Result<Option<PathBuf>, IntegrityError> {
    let mut tamper: Option<IntegrityError> = None;
    for _ in 0..2 {
        match download_and_verify(client, url, path, dir, expected) {
            Ok(true) => return Ok(Some(path.to_path_buf())),
            Ok(false) => {
                tamper = None;
                break;
            }
            Err(e) => tamper = Some(e), // подмена — повторяем, затем блок
        }
    }
    if let Some(e) = tamper {
        return Err(e);
    }
    // Сеть недоступна — кэш, но только если проходит сверку.
    if path.exists() {
        verify_sha(path, expected)?;
        return Ok(Some(path.to_path_buf()));
    }
    Ok(None)
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn sha_opt_empty_is_none() {
        assert_eq!(sha_opt(""), None);
        assert_eq!(sha_opt("abc"), Some("abc"));
    }

    #[test]
    fn verify_sha_without_expected_is_ok() {
        // Несуществующий путь, но ожидаемого хэша нет → сверка не требуется.
        assert!(verify_sha(Path::new("/nonexistent/x"), None).is_ok());
    }

    // Битая/подменённая загрузка не должна затирать уже проверенный файл на месте.
    #[test]
    fn install_verified_keeps_previous_file_on_mismatch() {
        let dir = std::env::temp_dir().join("pjm_install_verified_test");
        let _ = fs::create_dir_all(&dir);
        let path = dir.join("agent.jar");
        let tmp = path.with_extension("part");
        fs::write(&path, b"good").unwrap();
        fs::write(&tmp, b"evil").unwrap();

        let res = install_verified(&tmp, &path, Some("deadbeef"));
        assert!(matches!(res, Err(IntegrityError::Tampered(_))));
        assert_eq!(
            fs::read(&path).unwrap(),
            b"good",
            "старый файл должен остаться"
        );
        assert!(!tmp.exists(), "мусорный .part должен удаляться");
        let _ = fs::remove_dir_all(&dir);
    }

    #[test]
    fn verify_sha_mismatch_is_tampered() {
        // Несуществующий файл + ожидаемый хэш → ошибка чтения трактуется как подмена.
        let err = verify_sha(Path::new("/nonexistent/agent.jar"), Some("deadbeef"));
        assert!(matches!(err, Err(IntegrityError::Tampered(_))));
    }
}
