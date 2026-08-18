//! Быстрая доставка профиля одним `tar.zst`: resumable HTTP Range, SHA-256
//! архива и каждого файла, безопасная распаковка без платформенных путей.

use std::collections::{HashMap, HashSet};
use std::fs::{self, File, OpenOptions};
use std::io::{Read, Write};
use std::path::{Component, Path, PathBuf};

use reqwest::blocking::Client;
use reqwest::header::RANGE;
use reqwest::StatusCode;
use serde::Deserialize;
use sha2::{Digest, Sha256};

#[derive(Debug, Deserialize, Clone)]
#[serde(rename_all = "camelCase")]
pub(crate) struct BundleInfo {
    pub(crate) build_id: i32,
    pub(crate) format: String,
    pub(crate) download_url: String,
    pub(crate) hash_sha256: String,
    pub(crate) size: i64,
}

#[derive(Debug, Clone)]
pub(crate) struct FileSpec {
    pub(crate) path: String,
    pub(crate) hash_sha256: String,
    pub(crate) size: i64,
    pub(crate) executable: bool,
}

/// Для малого патча дешевле оставить параллельную пофайловую загрузку. Bundle
/// выигрывает на первой установке и массовом обновлении, где раньше RTT умножался
/// на тысячи файлов.
pub(crate) fn should_use(
    bundle: Option<&BundleInfo>,
    missing_files: usize,
    missing_bytes: u64,
) -> bool {
    bundle.is_some_and(|value| {
        value.format == "tar.zst"
            && value.size > 0
            && (missing_files >= 256 || missing_bytes.saturating_mul(2) >= value.size as u64)
    })
}

pub(crate) fn download_and_install(
    client: &Client,
    url: &str,
    token: &str,
    profile_root: &Path,
    files_root: &Path,
    bundle: &BundleInfo,
    files: &[FileSpec],
    progress: impl Fn(u64, u64),
) -> Result<(), String> {
    if bundle.format != "tar.zst" || bundle.size <= 0 || bundle.hash_sha256.len() != 64 {
        return Err("Backend вернул некорректное описание bundle.".to_string());
    }
    let downloads = profile_root.join(".downloads");
    fs::create_dir_all(&downloads).map_err(|_| "Не удалось создать папку загрузок.".to_string())?;
    let archive = downloads.join(format!("{}.tar.zst.part", bundle.build_id));

    let mut last_error = String::new();
    for attempt in 0..2 {
        if attempt > 0 {
            // Сетевой обрыв оставляет корректный partial — второй запрос продолжит
            // его через Range. Полный сброс нужен только после checksum mismatch.
        }
        if let Err(error) =
            download_resumable(client, url, token, &archive, bundle.size as u64, &progress)
        {
            last_error = error;
            continue;
        }
        match verify_file(&archive, &bundle.hash_sha256, bundle.size) {
            Ok(()) => {
                extract_verified(&archive, profile_root, files_root, bundle.build_id, files)?;
                let _ = fs::remove_file(&archive);
                return Ok(());
            }
            Err(error) => {
                last_error = error;
                let _ = fs::remove_file(&archive);
            }
        }
    }
    let _ = fs::remove_file(&archive);
    Err(last_error)
}

fn download_resumable(
    client: &Client,
    url: &str,
    token: &str,
    path: &Path,
    expected_size: u64,
    progress: &impl Fn(u64, u64),
) -> Result<(), String> {
    let existing = fs::metadata(path).map(|m| m.len()).unwrap_or(0);
    if existing > expected_size {
        fs::remove_file(path)
            .map_err(|_| "Не удалось сбросить повреждённую загрузку.".to_string())?;
    }
    let offset = fs::metadata(path).map(|m| m.len()).unwrap_or(0);
    if offset == expected_size {
        progress(offset, expected_size);
        return Ok(());
    }

    let mut request = client.get(url).bearer_auth(token);
    if offset > 0 {
        request = request.header(RANGE, format!("bytes={offset}-"));
    }
    let mut response = request
        .send()
        .map_err(|error| format!("Не удалось скачать bundle: {error}"))?;

    let append = offset > 0 && response.status() == StatusCode::PARTIAL_CONTENT;
    if !append && !response.status().is_success() {
        return Err(format!(
            "Ошибка скачивания bundle: HTTP {}",
            response.status().as_u16()
        ));
    }
    let mut output = OpenOptions::new()
        .create(true)
        .write(true)
        .append(append)
        .truncate(!append)
        .open(path)
        .map_err(|_| "Не удалось записать bundle.".to_string())?;
    let mut downloaded = if append { offset } else { 0 };
    let mut buffer = [0_u8; 256 * 1024];
    loop {
        let read = response
            .read(&mut buffer)
            .map_err(|error| format!("Соединение при скачивании bundle оборвалось: {error}"))?;
        if read == 0 {
            break;
        }
        output
            .write_all(&buffer[..read])
            .map_err(|_| "Не удалось записать bundle.".to_string())?;
        downloaded += read as u64;
        progress(downloaded.min(expected_size), expected_size);
    }
    output
        .flush()
        .map_err(|_| "Не удалось сохранить bundle.".to_string())?;
    if downloaded != expected_size {
        return Err(format!(
            "Bundle скачан не полностью: {downloaded} из {expected_size} байт. Повторите запуск для продолжения."
        ));
    }
    Ok(())
}

fn verify_file(path: &Path, expected_hash: &str, expected_size: i64) -> Result<(), String> {
    let metadata = fs::metadata(path).map_err(|_| "Скачанный bundle не найден.".to_string())?;
    if metadata.len() != expected_size as u64 {
        return Err("Размер bundle не совпадает с manifest.".to_string());
    }
    let mut file = File::open(path).map_err(|_| "Не удалось проверить bundle.".to_string())?;
    let mut hasher = Sha256::new();
    let mut buffer = [0_u8; 256 * 1024];
    loop {
        let read = file
            .read(&mut buffer)
            .map_err(|_| "Не удалось проверить bundle.".to_string())?;
        if read == 0 {
            break;
        }
        hasher.update(&buffer[..read]);
    }
    let actual = hex::encode(hasher.finalize());
    if !actual.eq_ignore_ascii_case(expected_hash) {
        return Err("SHA-256 bundle не совпадает с manifest.".to_string());
    }
    Ok(())
}

fn extract_verified(
    archive_path: &Path,
    profile_root: &Path,
    files_root: &Path,
    build_id: i32,
    files: &[FileSpec],
) -> Result<(), String> {
    let staging = profile_root.join(format!(".bundle-staging-{build_id}"));
    if staging.exists() {
        fs::remove_dir_all(&staging)
            .map_err(|_| "Не удалось очистить временную распаковку.".to_string())?;
    }
    fs::create_dir_all(&staging)
        .map_err(|_| "Не удалось создать временную распаковку.".to_string())?;

    let result = extract_to_staging(archive_path, &staging, files)
        .and_then(|_| install_staged(&staging, profile_root, files_root, build_id, files));
    let _ = fs::remove_dir_all(&staging);
    result
}

fn extract_to_staging(
    archive_path: &Path,
    staging: &Path,
    files: &[FileSpec],
) -> Result<(), String> {
    let expected: HashMap<&str, &FileSpec> = files
        .iter()
        .map(|file| (file.path.as_str(), file))
        .collect();
    let archive = File::open(archive_path).map_err(|_| "Не удалось открыть bundle.".to_string())?;
    let decoder = zstd::stream::read::Decoder::new(archive)
        .map_err(|_| "Bundle имеет некорректный zstd-формат.".to_string())?;
    let mut tar = tar::Archive::new(decoder);
    let mut seen = HashSet::with_capacity(files.len());
    let entries = tar
        .entries()
        .map_err(|_| "Bundle имеет некорректный tar-формат.".to_string())?;
    for entry in entries {
        let mut entry = entry.map_err(|_| "Не удалось прочитать запись bundle.".to_string())?;
        if !entry.header().entry_type().is_file() {
            return Err("Bundle содержит ссылку или специальный файл.".to_string());
        }
        let raw_path = entry
            .path()
            .map_err(|_| "Bundle содержит некорректный путь.".to_string())?
            .to_string_lossy()
            .replace('\\', "/");
        let normalized = normalize_relative(&raw_path)
            .ok_or_else(|| format!("Небезопасный путь в bundle: {raw_path}"))?;
        let spec = expected
            .get(normalized.as_str())
            .ok_or_else(|| format!("Лишний файл в bundle: {normalized}"))?;
        if !seen.insert(normalized.clone()) {
            return Err(format!("Файл повторяется в bundle: {normalized}"));
        }
        let target = safe_join(staging, &normalized)?;
        if let Some(parent) = target.parent() {
            fs::create_dir_all(parent)
                .map_err(|_| format!("Не удалось создать папку для {normalized}"))?;
        }
        let mut output =
            File::create(&target).map_err(|_| format!("Не удалось распаковать {normalized}"))?;
        let mut hasher = Sha256::new();
        let bytes = std::io::copy(
            &mut entry,
            &mut HashingWriter::new(&mut output, &mut hasher),
        )
        .map_err(|_| format!("Не удалось распаковать {normalized}"))?;
        output
            .flush()
            .map_err(|_| format!("Не удалось сохранить {normalized}"))?;
        if bytes != spec.size as u64
            || !hex::encode(hasher.finalize()).eq_ignore_ascii_case(&spec.hash_sha256)
        {
            return Err(format!("Контроль целостности не пройден: {normalized}"));
        }
        set_executable(&target, spec.executable)?;
    }
    if seen.len() != expected.len() {
        let missing = expected
            .keys()
            .find(|path| !seen.contains(**path))
            .copied()
            .unwrap_or("unknown");
        return Err(format!("В bundle отсутствует файл: {missing}"));
    }
    Ok(())
}

struct InstalledFile {
    target: PathBuf,
    backup: Option<PathBuf>,
}

/// Installs the already verified staging tree as one transaction. Existing
/// files move to a backup first; any failed rename restores every prior target.
fn install_staged(
    staging: &Path,
    profile_root: &Path,
    files_root: &Path,
    build_id: i32,
    files: &[FileSpec],
) -> Result<(), String> {
    let backup_root = profile_root.join(format!(".bundle-backup-{build_id}"));
    if backup_root.exists() {
        fs::remove_dir_all(&backup_root)
            .map_err(|_| "Не удалось очистить backup сборки.".to_string())?;
    }
    fs::create_dir_all(&backup_root)
        .map_err(|_| "Не удалось создать backup сборки.".to_string())?;

    let paths = files
        .iter()
        .map(|spec| {
            let source = safe_join(staging, &spec.path)?;
            let target = safe_join(files_root, &spec.path)?;
            let backup = safe_join(&backup_root, &spec.path)?;
            if let Some(parent) = target.parent() {
                fs::create_dir_all(parent)
                    .map_err(|_| format!("Не удалось создать папку для {}", spec.path))?;
            }
            if let Some(parent) = backup.parent() {
                fs::create_dir_all(parent)
                    .map_err(|_| format!("Не удалось создать backup для {}", spec.path))?;
            }
            Ok((source, target, backup))
        })
        .collect::<Result<Vec<_>, String>>()?;

    let mut installed = Vec::with_capacity(files.len());
    for (spec, (source, target, backup)) in files.iter().zip(paths) {
        let previous = match fs::symlink_metadata(&target) {
            Ok(_) => {
                if fs::rename(&target, &backup).is_err() {
                    rollback_install(&mut installed);
                    let _ = fs::remove_dir_all(&backup_root);
                    return Err(format!("Не удалось сохранить прежний {}", spec.path));
                }
                Some(backup)
            }
            Err(error) if error.kind() == std::io::ErrorKind::NotFound => None,
            Err(_) => {
                rollback_install(&mut installed);
                let _ = fs::remove_dir_all(&backup_root);
                return Err(format!("Не удалось проверить {}", spec.path));
            }
        };

        if fs::rename(&source, &target).is_err() {
            if let Some(path) = previous.as_ref() {
                let _ = fs::rename(path, &target);
            }
            rollback_install(&mut installed);
            let _ = fs::remove_dir_all(&backup_root);
            return Err(format!("Не удалось установить {}", spec.path));
        }
        installed.push(InstalledFile {
            target,
            backup: previous,
        });
    }

    let _ = fs::remove_dir_all(&backup_root);
    Ok(())
}

fn rollback_install(installed: &mut Vec<InstalledFile>) {
    for item in installed.drain(..).rev() {
        let _ = remove_existing(&item.target);
        if let Some(backup) = item.backup {
            let _ = fs::rename(backup, item.target);
        }
    }
}

fn set_executable(
    #[cfg_attr(not(unix), allow(unused_variables))] path: &Path,
    executable: bool,
) -> Result<(), String> {
    #[cfg(unix)]
    {
        use std::os::unix::fs::PermissionsExt;

        let metadata =
            fs::metadata(path).map_err(|_| "Не удалось проверить права файла.".to_string())?;
        let mut permissions = metadata.permissions();
        let mode = if executable {
            permissions.mode() | 0o111
        } else {
            permissions.mode() & !0o111
        };
        permissions.set_mode(mode);
        fs::set_permissions(path, permissions)
            .map_err(|_| "Не удалось выставить права файла.".to_string())?;
    }
    Ok(())
}

struct HashingWriter<'a, W: Write> {
    writer: &'a mut W,
    hasher: &'a mut Sha256,
}

impl<'a, W: Write> HashingWriter<'a, W> {
    fn new(writer: &'a mut W, hasher: &'a mut Sha256) -> Self {
        Self { writer, hasher }
    }
}

impl<W: Write> Write for HashingWriter<'_, W> {
    fn write(&mut self, buf: &[u8]) -> std::io::Result<usize> {
        let written = self.writer.write(buf)?;
        self.hasher.update(&buf[..written]);
        Ok(written)
    }

    fn flush(&mut self) -> std::io::Result<()> {
        self.writer.flush()
    }
}

fn normalize_relative(value: &str) -> Option<String> {
    let mut result = Vec::new();
    let portable = value.replace('\\', "/");
    if portable.starts_with('/') || portable.contains(':') {
        return None;
    }
    for component in Path::new(&portable).components() {
        match component {
            Component::Normal(part) => result.push(part.to_string_lossy().to_string()),
            Component::CurDir => {}
            _ => return None,
        }
    }
    (!result.is_empty()).then(|| result.join("/"))
}

fn safe_join(root: &Path, rel: &str) -> Result<PathBuf, String> {
    let normalized = normalize_relative(rel).ok_or_else(|| format!("Небезопасный путь: {rel}"))?;
    let path = normalized
        .split('/')
        .fold(root.to_path_buf(), |path, part| path.join(part));
    let root_abs = root
        .canonicalize()
        .map_err(|_| "Не удалось проверить папку профиля.".to_string())?;
    let mut existing = path.as_path();
    while !existing.exists() {
        existing = existing
            .parent()
            .ok_or_else(|| format!("Путь выходит за папку профиля: {rel}"))?;
    }
    let existing_abs = existing
        .canonicalize()
        .map_err(|_| format!("Не удалось проверить путь: {rel}"))?;
    if existing_abs != root_abs && !existing_abs.starts_with(&root_abs) {
        return Err(format!("Путь выходит за папку профиля: {rel}"));
    }
    Ok(path)
}

fn remove_existing(path: &Path) -> std::io::Result<()> {
    match fs::symlink_metadata(path) {
        Ok(metadata) if metadata.is_dir() && !metadata.file_type().is_symlink() => {
            fs::remove_dir_all(path)
        }
        Ok(_) => fs::remove_file(path),
        Err(error) if error.kind() == std::io::ErrorKind::NotFound => Ok(()),
        Err(error) => Err(error),
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    use std::time::{SystemTime, UNIX_EPOCH};

    #[test]
    fn bundle_is_only_used_for_mass_downloads() {
        let bundle = BundleInfo {
            build_id: 1,
            format: "tar.zst".to_string(),
            download_url: "/bundle".to_string(),
            hash_sha256: "0".repeat(64),
            size: 100,
        };
        assert!(!should_use(Some(&bundle), 31, 1));
        assert!(should_use(Some(&bundle), 256, 1));
        assert!(should_use(Some(&bundle), 1, 50));
        assert!(!should_use(None, 1000, 1000));
    }

    #[test]
    fn cross_platform_paths_are_normalized_and_traversal_is_rejected() {
        assert_eq!(
            normalize_relative("mods/example.jar").as_deref(),
            Some("mods/example.jar")
        );
        assert_eq!(
            normalize_relative("mods\\example.jar").as_deref(),
            Some("mods/example.jar")
        );
        assert!(normalize_relative("../escape.jar").is_none());
        assert!(normalize_relative("/absolute.jar").is_none());
        assert!(normalize_relative("C:/escape.jar").is_none());
    }

    #[test]
    fn extraction_verifies_every_file_before_replacing_installation() {
        let root = test_root("verified-extraction");
        let files_root = root.join("files");
        fs::create_dir_all(files_root.join("mods")).unwrap();
        fs::write(files_root.join("mods/old.jar"), b"working-old").unwrap();
        let archive = root.join("bundle.tar.zst");
        write_bundle(
            &archive,
            &[("mods/old.jar", b"new"), ("mods/bad.jar", b"bad")],
        );
        let specs = vec![
            FileSpec {
                path: "mods/old.jar".to_string(),
                hash_sha256: hex::encode(Sha256::digest(b"new")),
                size: 3,
                executable: false,
            },
            FileSpec {
                path: "mods/bad.jar".to_string(),
                hash_sha256: "0".repeat(64),
                size: 3,
                executable: false,
            },
        ];

        assert!(extract_verified(&archive, &root, &files_root, 1, &specs).is_err());
        assert_eq!(
            fs::read(files_root.join("mods/old.jar")).unwrap(),
            b"working-old"
        );
        assert!(!files_root.join("mods/bad.jar").exists());
        let _ = fs::remove_dir_all(root);
    }

    #[test]
    fn failed_install_restores_files_already_replaced() {
        let root = test_root("transaction-rollback");
        let staging = root.join("staging");
        let files_root = root.join("files");
        fs::create_dir_all(staging.join("mods")).unwrap();
        fs::create_dir_all(files_root.join("mods")).unwrap();
        fs::write(staging.join("mods/first.jar"), b"new-first").unwrap();
        fs::write(files_root.join("mods/first.jar"), b"old-first").unwrap();
        fs::write(files_root.join("mods/second.jar"), b"old-second").unwrap();
        let specs = vec![
            FileSpec {
                path: "mods/first.jar".to_string(),
                hash_sha256: String::new(),
                size: 0,
                executable: false,
            },
            FileSpec {
                path: "mods/second.jar".to_string(),
                hash_sha256: String::new(),
                size: 0,
                executable: false,
            },
        ];

        assert!(install_staged(&staging, &root, &files_root, 7, &specs).is_err());
        assert_eq!(
            fs::read(files_root.join("mods/first.jar")).unwrap(),
            b"old-first"
        );
        assert_eq!(
            fs::read(files_root.join("mods/second.jar")).unwrap(),
            b"old-second"
        );
        let _ = fs::remove_dir_all(root);
    }

    #[cfg(unix)]
    #[test]
    fn executable_flag_is_restored_from_manifest() {
        use std::os::unix::fs::PermissionsExt;

        let root = test_root("executable");
        let files_root = root.join("files");
        fs::create_dir_all(&files_root).unwrap();
        let archive = root.join("bundle.tar.zst");
        write_bundle(&archive, &[("bin/start", b"run")]);
        let specs = vec![FileSpec {
            path: "bin/start".to_string(),
            hash_sha256: hex::encode(Sha256::digest(b"run")),
            size: 3,
            executable: true,
        }];

        extract_verified(&archive, &root, &files_root, 2, &specs).unwrap();
        let mode = fs::metadata(files_root.join("bin/start"))
            .unwrap()
            .permissions()
            .mode();
        assert_ne!(mode & 0o111, 0);
        let _ = fs::remove_dir_all(root);
    }

    fn test_root(name: &str) -> PathBuf {
        let nanos = SystemTime::now()
            .duration_since(UNIX_EPOCH)
            .unwrap()
            .as_nanos();
        let root = std::env::temp_dir().join(format!(
            "launcher-bundle-{}-{name}-{nanos}",
            std::process::id()
        ));
        fs::create_dir_all(&root).unwrap();
        root
    }

    fn write_bundle(path: &Path, entries: &[(&str, &[u8])]) {
        let file = File::create(path).unwrap();
        let encoder = zstd::stream::write::Encoder::new(file, 1).unwrap();
        let mut tar = tar::Builder::new(encoder);
        for (name, data) in entries {
            let mut header = tar::Header::new_gnu();
            header.set_path(name).unwrap();
            header.set_size(data.len() as u64);
            header.set_mode(0o644);
            header.set_cksum();
            tar.append(&header, *data).unwrap();
        }
        let encoder = tar.into_inner().unwrap();
        encoder.finish().unwrap();
    }
}
