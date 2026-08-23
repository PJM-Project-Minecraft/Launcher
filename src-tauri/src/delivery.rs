//! Delivery protocol v2. One content-addressed transfer engine is shared by
//! profile installation and launcher self-update.

use std::collections::HashMap;
use std::fs::{self, File};
use std::io::Write;
use std::path::{Component, Path, PathBuf};
use std::sync::atomic::{AtomicUsize, Ordering};
use std::sync::{Arc, LazyLock, Mutex};
use std::thread;
use std::time::Duration;

use ed25519_dalek::{Signature, VerifyingKey};
use rayon::prelude::*;
use reqwest::blocking::Client;
use serde::{Deserialize, Serialize};
use sha2::{Digest, Sha256};

pub const SCHEMA_VERSION: i32 = 2;
const PROFILE_IO_PARALLELISM: usize = 8;
static CHUNK_DOWNLOAD_LOCKS: LazyLock<Mutex<HashMap<String, Arc<Mutex<()>>>>> =
    LazyLock::new(|| Mutex::new(HashMap::new()));
static CHUNK_TEMP_SEQUENCE: AtomicUsize = AtomicUsize::new(0);

#[derive(Debug, Clone, Deserialize, Serialize, Default)]
#[serde(rename_all = "camelCase")]
pub struct ChunkRef {
    pub sha256: String,
    pub size: i64,
}

#[derive(Debug, Clone, Deserialize, Serialize, Default)]
#[serde(rename_all = "camelCase")]
pub struct ReleaseFile {
    pub path: String,
    pub size: i64,
    pub sha256: String,
    #[serde(default)]
    pub executable: bool,
    pub chunks: Vec<ChunkRef>,
}

#[derive(Debug, Clone, Deserialize)]
#[serde(rename_all = "camelCase")]
pub struct ProfileConfig {
    pub id: String,
    pub name: String,
    #[serde(default)]
    pub java_version: i32,
    #[serde(default)]
    pub jvm_args: String,
    #[serde(default)]
    pub java_path_windows: String,
    #[serde(default)]
    pub java_path_linux: String,
    #[serde(default)]
    pub java_path_macos: String,
    #[serde(default)]
    pub launch_command_windows: String,
    #[serde(default)]
    pub launch_command_linux: String,
    #[serde(default)]
    pub launch_command_macos: String,
    #[serde(default)]
    pub preserve_paths: Vec<String>,
}

#[derive(Debug, Clone, Deserialize)]
#[serde(rename_all = "camelCase")]
pub struct ProfileManifest {
    pub schema_version: i32,
    pub kind: String,
    pub release_id: String,
    pub sequence: i32,
    pub profile: ProfileConfig,
    pub files: Vec<ReleaseFile>,
    pub file_count: usize,
    pub total_size: i64,
}

#[derive(Debug, Clone, Deserialize)]
#[serde(rename_all = "camelCase")]
pub struct LauncherManifest {
    pub schema_version: i32,
    pub kind: String,
    pub release_id: String,
    pub version: String,
    pub platform: String,
    #[serde(default)]
    pub mandatory: bool,
    #[serde(default)]
    pub changelog: String,
    #[serde(default)]
    pub artifact_signature: String,
    pub artifact: ReleaseFile,
}

#[derive(Clone)]
pub enum Scope {
    Profile { profile_id: String },
    Launcher { release_id: String },
}

impl Scope {
    fn chunk_url(&self, api_url: &str, hash: &str) -> String {
        match self {
            Scope::Profile { profile_id } => format!(
                "{}/api/v2/profiles/{}/chunks/{}",
                api_url.trim_end_matches('/'),
                profile_id,
                hash
            ),
            Scope::Launcher { release_id } => format!(
                "{}/api/v2/launcher/releases/{}/chunks/{}",
                api_url.trim_end_matches('/'),
                release_id,
                hash
            ),
        }
    }
}

pub fn fetch_profile_manifest(
    client: &Client,
    api_url: &str,
    token: &str,
    profile_id: &str,
    release_id: &str,
    expected_sha256: &str,
    expected_signature: &str,
) -> Result<ProfileManifest, String> {
    let url = format!(
        "{}/api/v2/profiles/{}/releases/{}/manifest",
        api_url.trim_end_matches('/'),
        profile_id,
        release_id
    );
    let response = client
        .get(url)
        .bearer_auth(token)
        .send()
        .map_err(|error| format!("Manifest v2 недоступен: {error}"))?;
    if !response.status().is_success() {
        return Err(format!("Manifest v2: HTTP {}", response.status().as_u16()));
    }
    let bytes = response
        .bytes()
        .map_err(|error| format!("Manifest v2 оборван: {error}"))?;
    verify_bytes(&bytes, expected_sha256, "manifest")?;
    verify_manifest_signature(&bytes, expected_signature)?;
    let manifest: ProfileManifest = serde_json::from_slice(&bytes)
        .map_err(|error| format!("Manifest v2 повреждён: {error}"))?;
    if manifest.schema_version != SCHEMA_VERSION
        || manifest.kind != "profile"
        || manifest.profile.id != profile_id
        || manifest.release_id != release_id
    {
        return Err("Backend вернул manifest v2 другого профиля или версии.".to_string());
    }
    validate_release_files(&manifest.files)?;
    Ok(manifest)
}

pub fn fetch_launcher_manifest(
    client: &Client,
    api_url: &str,
    platform: &str,
    current_version: &str,
) -> Result<Option<LauncherManifest>, String> {
    let url = format!(
        "{}/api/v2/launcher/releases/current?platform={}&from={}",
        api_url.trim_end_matches('/'),
        platform,
        current_version
    );
    let response = client
        .get(url)
        .send()
        .map_err(|error| format!("Сервер обновлений v2 недоступен: {error}"))?;
    if response.status() == reqwest::StatusCode::NO_CONTENT {
        return Ok(None);
    }
    if !response.status().is_success() {
        return Err(format!(
            "Проверка обновлений v2: HTTP {}",
            response.status().as_u16()
        ));
    }
    let expected_sha256 = response
        .headers()
        .get("X-Manifest-SHA256")
        .and_then(|value| value.to_str().ok())
        .unwrap_or_default()
        .to_string();
    let signature = response
        .headers()
        .get("X-Manifest-Signature")
        .and_then(|value| value.to_str().ok())
        .unwrap_or_default()
        .to_string();
    let mandatory = response
        .headers()
        .get("X-Update-Mandatory")
        .and_then(|value| value.to_str().ok())
        .map(|value| value.eq_ignore_ascii_case("true"))
        .unwrap_or(false);
    let body = response
        .bytes()
        .map_err(|error| format!("Ответ обновлений v2 оборван: {error}"))?;
    verify_bytes(&body, &expected_sha256, "launcher manifest")?;
    verify_manifest_signature(&body, &signature)?;
    let mut manifest: LauncherManifest = serde_json::from_slice(&body)
        .map_err(|error| format!("Ответ обновлений v2 повреждён: {error}"))?;
    if manifest.schema_version != SCHEMA_VERSION
        || manifest.kind != "launcher"
        || manifest.platform != platform
    {
        return Err("Backend вернул несовместимый launcher manifest v2.".to_string());
    }
    validate_release_files(std::slice::from_ref(&manifest.artifact))?;
    manifest.mandatory = mandatory;
    Ok(Some(manifest))
}

pub fn reconstruct_file(
    client: &crate::DownloadClient,
    api_url: &str,
    token: Option<&str>,
    scope: &Scope,
    cache_root: &Path,
    spec: &ReleaseFile,
    destination: &Path,
    progress: &(dyn Fn(usize, usize) + Sync),
) -> Result<u64, String> {
    fs::create_dir_all(cache_root).map_err(|_| "Не удалось создать chunk-cache.".to_string())?;
    // One file can reference the same content chunk more than once. Download each
    // hash once: parallel workers must never truncate/rename the same cache entry.
    let mut seen = std::collections::HashSet::new();
    let unique_chunks = spec
        .chunks
        .iter()
        .filter(|chunk| seen.insert(chunk.sha256.as_str()))
        .collect::<Vec<_>>();
    let completed = AtomicUsize::new(0);
    let total = unique_chunks.len();
    unique_chunks.par_iter().try_for_each(|chunk| {
        ensure_chunk(client, api_url, token, scope, cache_root, chunk)?;
        progress(completed.fetch_add(1, Ordering::Relaxed) + 1, total);
        Ok::<(), String>(())
    })?;
    if let Some(parent) = destination.parent() {
        fs::create_dir_all(parent)
            .map_err(|_| format!("Не удалось создать папку для {}", spec.path))?;
    }
    let temp = destination.with_extension(format!("delivery-{}.part", std::process::id()));
    let result = (|| {
        let mut output =
            File::create(&temp).map_err(|_| format!("Не удалось подготовить {}", spec.path))?;
        let mut whole = Sha256::new();
        let mut written = 0_u64;
        for chunk in &spec.chunks {
            let path = chunk_path(cache_root, &chunk.sha256)?;
            let mut input =
                File::open(path).map_err(|_| format!("Chunk {} исчез из cache", chunk.sha256))?;
            let copied = std::io::copy(&mut input, &mut output)
                .map_err(|_| format!("Не удалось собрать {}", spec.path))?;
            written += copied;
        }
        output
            .flush()
            .map_err(|_| format!("Не удалось сохранить {}", spec.path))?;
        let mut check =
            File::open(&temp).map_err(|_| format!("Не удалось проверить {}", spec.path))?;
        std::io::copy(&mut check, &mut whole)
            .map_err(|_| format!("Не удалось проверить {}", spec.path))?;
        let actual = format!("{:x}", whole.finalize());
        if written != spec.size.max(0) as u64 || actual != spec.sha256.to_lowercase() {
            return Err(format!(
                "Итоговый файл {} не совпадает с manifest v2",
                spec.path
            ));
        }
        set_executable(&temp, spec.executable)?;
        if destination.exists() {
            if destination.is_dir() {
                fs::remove_dir_all(destination)
                    .map_err(|_| format!("Не удалось заменить {}", spec.path))?;
            } else {
                fs::remove_file(destination)
                    .map_err(|_| format!("Не удалось заменить {}", spec.path))?;
            }
        }
        fs::rename(&temp, destination)
            .map_err(|_| format!("Не удалось установить {}", spec.path))?;
        Ok(written)
    })();
    if result.is_err() {
        let _ = fs::remove_file(temp);
    }
    result
}

fn ensure_chunk(
    client: &crate::DownloadClient,
    api_url: &str,
    token: Option<&str>,
    scope: &Scope,
    cache_root: &Path,
    chunk: &ChunkRef,
) -> Result<(), String> {
    let target = chunk_path(cache_root, &chunk.sha256)?;
    let chunk_lock = {
        let mut locks = CHUNK_DOWNLOAD_LOCKS
            .lock()
            .unwrap_or_else(|error| error.into_inner());
        Arc::clone(
            locks
                .entry(chunk.sha256.clone())
                .or_insert_with(|| Arc::new(Mutex::new(()))),
        )
    };
    let _chunk_guard = chunk_lock.lock().unwrap_or_else(|error| error.into_inner());
    if verify_path(&target, &chunk.sha256, chunk.size).is_ok() {
        return Ok(());
    }
    let _ = fs::remove_file(&target);
    if let Some(parent) = target.parent() {
        fs::create_dir_all(parent).map_err(|_| "Не удалось создать chunk-cache.".to_string())?;
    }
    let mut last_error = String::new();
    for attempt in 0..3 {
        let response = client.get(&scope.chunk_url(api_url, &chunk.sha256), token, None);
        match response {
            Ok(response) if response.status().is_success() => {
                let sequence = CHUNK_TEMP_SEQUENCE.fetch_add(1, Ordering::Relaxed);
                let temp =
                    target.with_extension(format!("{}.{}.part", std::process::id(), sequence));
                let attempt_result = (|| {
                    let mut output = File::create(&temp)
                        .map_err(|_| "Не удалось подготовить chunk.".to_string())?;
                    let mut hash = Sha256::new();
                    let mut received = 0_i64;
                    response.consume(|bytes| {
                        output
                            .write_all(bytes)
                            .map_err(|_| "Не удалось записать chunk.".to_string())?;
                        hash.update(bytes);
                        received += bytes.len() as i64;
                        Ok(())
                    })?;
                    output
                        .flush()
                        .map_err(|_| "Не удалось сохранить chunk.".to_string())?;
                    if received != chunk.size
                        || format!("{:x}", hash.finalize()) != chunk.sha256.to_lowercase()
                    {
                        return Err("Backend вернул повреждённый chunk.".to_string());
                    }
                    fs::rename(&temp, &target)
                        .map_err(|_| "Не удалось сохранить chunk.".to_string())?;
                    Ok(())
                })();
                if attempt_result.is_ok() {
                    return Ok(());
                }
                let _ = fs::remove_file(temp);
                last_error = attempt_result.unwrap_err();
            }
            Ok(response) => last_error = format!("Chunk HTTP {}", response.status().as_u16()),
            Err(error) => last_error = format!("Chunk недоступен: {error}"),
        }
        if attempt < 2 {
            thread::sleep(Duration::from_millis(500_u64 << attempt));
        }
    }
    Err(last_error)
}

fn validate_release_files(files: &[ReleaseFile]) -> Result<(), String> {
    let mut seen = std::collections::HashSet::new();
    for file in files {
        safe_relative(&file.path)?;
        if !seen.insert(file.path.to_lowercase()) {
            return Err(format!(
                "Manifest содержит конфликтующий путь: {}",
                file.path
            ));
        }
        let chunk_size: i64 = file.chunks.iter().map(|chunk| chunk.size).sum();
        if chunk_size != file.size
            || file.sha256.len() != 64
            || file
                .chunks
                .iter()
                .any(|chunk| chunk.sha256.len() != 64 || chunk.size <= 0)
        {
            return Err(format!(
                "Manifest содержит некорректный файл: {}",
                file.path
            ));
        }
    }
    Ok(())
}

fn safe_relative(value: &str) -> Result<PathBuf, String> {
    let path = Path::new(value);
    if value.is_empty() || path.is_absolute() {
        return Err(format!("Небезопасный путь в manifest v2: {value}"));
    }
    let mut result = PathBuf::new();
    for component in path.components() {
        match component {
            Component::Normal(part) => result.push(part),
            _ => return Err(format!("Небезопасный путь в manifest v2: {value}")),
        }
    }
    Ok(result)
}

fn chunk_path(root: &Path, hash: &str) -> Result<PathBuf, String> {
    if hash.len() != 64
        || !hash
            .bytes()
            .all(|byte| byte.is_ascii_digit() || (b'a'..=b'f').contains(&byte))
    {
        return Err("Некорректный SHA-256 chunk.".to_string());
    }
    Ok(root.join(&hash[..2]).join(hash))
}

fn verify_path(path: &Path, expected: &str, size: i64) -> Result<(), String> {
    let metadata = fs::symlink_metadata(path).map_err(|_| "Chunk отсутствует.".to_string())?;
    if !metadata.is_file() || metadata.file_type().is_symlink() || metadata.len() as i64 != size {
        return Err("Размер chunk не совпадает.".to_string());
    }
    let mut input = File::open(path).map_err(|_| "Chunk отсутствует.".to_string())?;
    let mut hash = Sha256::new();
    std::io::copy(&mut input, &mut hash)
        .map_err(|_| "Не удалось проверить SHA-256 chunk.".to_string())?;
    if format!("{:x}", hash.finalize()) != expected.to_lowercase() {
        return Err("SHA-256 chunk не совпадает.".to_string());
    }
    Ok(())
}

fn verify_bytes(data: &[u8], expected: &str, label: &str) -> Result<(), String> {
    let actual = format!("{:x}", Sha256::digest(data));
    if actual != expected.to_lowercase() {
        return Err(format!("SHA-256 {label} не совпадает."));
    }
    Ok(())
}

fn verify_manifest_signature(data: &[u8], signature: &str) -> Result<(), String> {
    let key_hex = match option_env!("DELIVERY_MANIFEST_PUBKEY")
        .map(str::trim)
        .filter(|value| !value.is_empty())
    {
        Some(value) => value,
        None if cfg!(debug_assertions) => return Ok(()),
        None => {
            return Err("Release-сборка не содержит ключ подписи delivery manifest.".to_string())
        }
    };
    let key_bytes =
        hex::decode(key_hex).map_err(|_| "В сборке повреждён ключ manifest v2.".to_string())?;
    let key_array: [u8; 32] = key_bytes
        .try_into()
        .map_err(|_| "В сборке повреждён ключ manifest v2.".to_string())?;
    let key = VerifyingKey::from_bytes(&key_array)
        .map_err(|_| "В сборке повреждён ключ manifest v2.".to_string())?;
    let signature_bytes =
        hex::decode(signature.trim()).map_err(|_| "Manifest v2 не подписан.".to_string())?;
    let signature_array: [u8; 64] = signature_bytes
        .try_into()
        .map_err(|_| "Manifest v2 не подписан.".to_string())?;
    key.verify_strict(data, &Signature::from_bytes(&signature_array))
        .map_err(|_| "Подпись manifest v2 недействительна.".to_string())
}

#[cfg(unix)]
fn set_executable(path: &Path, executable: bool) -> Result<(), String> {
    use std::os::unix::fs::PermissionsExt;
    let mode = if executable { 0o755 } else { 0o644 };
    fs::set_permissions(path, fs::Permissions::from_mode(mode))
        .map_err(|_| "Не удалось выставить права файла.".to_string())
}

#[cfg(not(unix))]
fn set_executable(_path: &Path, _executable: bool) -> Result<(), String> {
    Ok(())
}

#[derive(Debug, Serialize, Deserialize)]
#[serde(rename_all = "camelCase")]
struct SwapJournal {
    release_id: String,
    phase: String,
    preserve_paths: Vec<String>,
}

pub fn install_profile(
    client: &crate::DownloadClient,
    api_url: &str,
    token: &str,
    profile_root: &Path,
    files_root: &Path,
    manifest: &ProfileManifest,
    progress: impl Fn(usize, usize) + Sync,
) -> Result<(), String> {
    recover_profile_swap(profile_root, files_root)?;
    validate_release_id(&manifest.release_id)?;
    let staging = profile_root.join(format!(".delivery-staging-{}", manifest.release_id));
    let backup = profile_root.join(format!(".delivery-backup-{}", manifest.release_id));
    if staging.exists() {
        fs::remove_dir_all(&staging).map_err(|_| "Не удалось очистить staging.".to_string())?;
    }
    if backup.exists() {
        fs::remove_dir_all(&backup)
            .map_err(|_| "Не удалось очистить старый backup.".to_string())?;
    }
    fs::create_dir_all(&staging).map_err(|_| "Не удалось создать staging профиля.".to_string())?;
    let cache = profile_root.join("chunks-v2");
    let scope = Scope::Profile {
        profile_id: manifest.profile.id.clone(),
    };
    let progress_units = manifest
        .files
        .iter()
        .map(file_progress_units)
        .collect::<Vec<_>>();
    let total = progress_units.iter().sum::<usize>();
    let completed = Mutex::new(0_usize);
    let report = |delta: usize| {
        let mut current = completed.lock().unwrap_or_else(|error| error.into_inner());
        *current += delta;
        progress(*current, total);
    };
    let pool = rayon::ThreadPoolBuilder::new()
        .num_threads(PROFILE_IO_PARALLELISM)
        .thread_name(|index| format!("profile-io-{index}"))
        .build()
        .map_err(|_| "Не удалось запустить очередь скачивания.".to_string())?;
    pool.install(|| {
        manifest
            .files
            .par_iter()
            .zip(progress_units.par_iter())
            .try_for_each(|(file, &units)| {
                let relative = safe_relative(&file.path)?;
                let existing = files_root.join(&relative);
                let target = staging.join(&relative);
                if verify_path(&existing, &file.sha256, file.size).is_ok() {
                    if let Some(parent) = target.parent() {
                        fs::create_dir_all(parent)
                            .map_err(|_| "Не удалось создать staging.".to_string())?;
                    }
                    if fs::hard_link(&existing, &target).is_err() {
                        fs::copy(&existing, &target)
                            .map_err(|_| format!("Не удалось перенести {}", file.path))?;
                    }
                    set_executable(&target, file.executable)?;
                    report(units);
                } else {
                    reconstruct_file(
                        client,
                        api_url,
                        Some(token),
                        &scope,
                        &cache,
                        file,
                        &target,
                        &|_, _| report(1),
                    )?;
                    if file.chunks.is_empty() {
                        report(1);
                    }
                }
                Ok::<(), String>(())
            })
    })?;
    let journal_path = profile_root.join("delivery-swap.json");
    write_journal(&journal_path, manifest, "prepared")?;
    if files_root.exists() {
        write_journal(&journal_path, manifest, "backingUp")?;
        fs::rename(files_root, &backup)
            .map_err(|_| "Не удалось сохранить предыдущую сборку.".to_string())?;
    }
    write_journal(&journal_path, manifest, "backedUp")?;
    write_journal(&journal_path, manifest, "preserving")?;
    if let Err(error) = move_preserved(&backup, &staging, &manifest.profile.preserve_paths)
        .and_then(|_| {
            write_journal(&journal_path, manifest, "activating")?;
            fs::rename(&staging, files_root)
                .map_err(|_| "Не удалось активировать новую сборку.".to_string())
        })
    {
        if files_root.exists() {
            let _ = fs::remove_dir_all(files_root);
        }
        if backup.exists() {
            let _ = move_preserved(&staging, &backup, &manifest.profile.preserve_paths);
            let _ = fs::rename(&backup, files_root);
        }
        return Err(error);
    }
    write_journal(&journal_path, manifest, "activated")?;
    if backup.exists() {
        fs::remove_dir_all(&backup)
            .map_err(|_| "Не удалось удалить backup после активации.".to_string())?;
    }
    let _ = fs::remove_file(journal_path);
    Ok(())
}

fn file_progress_units(file: &ReleaseFile) -> usize {
    file.chunks
        .iter()
        .map(|chunk| chunk.sha256.as_str())
        .collect::<std::collections::HashSet<_>>()
        .len()
        .max(1)
}

fn move_preserved(backup: &Path, staging: &Path, paths: &[String]) -> Result<(), String> {
    if !backup.exists() {
        return Ok(());
    }
    for value in paths {
        let normalized = value.trim_end_matches('/');
        if normalized.is_empty() {
            continue;
        }
        let relative = safe_relative(normalized)?;
        let source = backup.join(&relative);
        if !source.exists() {
            continue;
        }
        let destination = staging.join(&relative);
        if destination.exists() {
            return Err(format!(
                "Preserve path конфликтует с managed-файлом: {normalized}"
            ));
        }
        if let Some(parent) = destination.parent() {
            fs::create_dir_all(parent)
                .map_err(|_| "Не удалось перенести пользовательские файлы.".to_string())?;
        }
        fs::rename(source, destination)
            .map_err(|_| format!("Не удалось сохранить {normalized}"))?;
    }
    Ok(())
}

fn write_journal(path: &Path, manifest: &ProfileManifest, phase: &str) -> Result<(), String> {
    let journal = SwapJournal {
        release_id: manifest.release_id.clone(),
        phase: phase.to_string(),
        preserve_paths: manifest.profile.preserve_paths.clone(),
    };
    let data =
        serde_json::to_vec(&journal).map_err(|_| "Не удалось создать swap-журнал.".to_string())?;
    let temp = path.with_extension("json.part");
    let mut file =
        File::create(&temp).map_err(|_| "Не удалось подготовить swap-журнал.".to_string())?;
    file.write_all(&data)
        .and_then(|_| file.sync_all())
        .map_err(|_| "Не удалось сохранить swap-журнал.".to_string())?;
    fs::rename(temp, path).map_err(|_| "Не удалось активировать swap-журнал.".to_string())
}

pub fn recover_profile_swap(profile_root: &Path, files_root: &Path) -> Result<(), String> {
    let path = profile_root.join("delivery-swap.json");
    let Ok(data) = fs::read(&path) else {
        return Ok(());
    };
    let journal: SwapJournal = serde_json::from_slice(&data)
        .map_err(|_| "Swap-журнал повреждён; требуется ручная проверка.".to_string())?;
    validate_release_id(&journal.release_id)?;
    let staging = profile_root.join(format!(".delivery-staging-{}", journal.release_id));
    let backup = profile_root.join(format!(".delivery-backup-{}", journal.release_id));
    match journal.phase.as_str() {
        "prepared" => {
            if staging.exists() {
                fs::remove_dir_all(staging)
                    .map_err(|_| "Не удалось очистить незавершённый staging.".to_string())?;
            }
        }
        "backingUp" | "backedUp" | "preserving" => {
            if backup.exists() {
                move_preserved(&staging, &backup, &journal.preserve_paths)?;
                if !files_root.exists() {
                    fs::rename(&backup, files_root)
                        .map_err(|_| "Не удалось восстановить предыдущую сборку.".to_string())?;
                }
            }
            if staging.exists() {
                let _ = fs::remove_dir_all(staging);
            }
        }
        "activating" => {
            if files_root.exists() {
                if backup.exists() {
                    fs::remove_dir_all(&backup)
                        .map_err(|_| "Не удалось завершить очистку backup.".to_string())?;
                }
            } else {
                if backup.exists() {
                    move_preserved(&staging, &backup, &journal.preserve_paths)?;
                    fs::rename(&backup, files_root)
                        .map_err(|_| "Не удалось восстановить предыдущую сборку.".to_string())?;
                }
                if staging.exists() {
                    let _ = fs::remove_dir_all(&staging);
                }
            }
        }
        "activated" => {
            if backup.exists() {
                fs::remove_dir_all(backup)
                    .map_err(|_| "Не удалось завершить очистку backup.".to_string())?;
            }
        }
        _ => return Err("Неизвестная фаза swap-журнала; требуется ручная проверка.".to_string()),
    }
    fs::remove_file(path).map_err(|_| "Не удалось закрыть swap-журнал.".to_string())
}

fn validate_release_id(value: &str) -> Result<(), String> {
    if value.is_empty()
        || value.len() > 64
        || !value
            .bytes()
            .all(|byte| byte.is_ascii_alphanumeric() || byte == b'-')
    {
        return Err("Некорректный release id в delivery manifest.".to_string());
    }
    Ok(())
}

#[cfg(test)]
mod tests {
    use super::*;
    use std::io::{Read, Write};
    use std::net::TcpListener;
    use std::sync::{Arc, Mutex};

    fn test_root(name: &str) -> PathBuf {
        let unique = std::time::SystemTime::now()
            .duration_since(std::time::UNIX_EPOCH)
            .unwrap()
            .as_nanos();
        std::env::temp_dir().join(format!(
            "pjm-delivery-{name}-{}-{unique}",
            std::process::id()
        ))
    }

    #[test]
    fn rejects_parent_and_absolute_paths() {
        assert!(safe_relative("mods/example.jar").is_ok());
        assert!(safe_relative("../secret").is_err());
        assert!(safe_relative("/etc/passwd").is_err());
    }

    #[test]
    fn validates_chunk_sizes() {
        let file = ReleaseFile {
            path: "mods/a.jar".into(),
            size: 4,
            sha256: "a".repeat(64),
            executable: false,
            chunks: vec![ChunkRef {
                sha256: "b".repeat(64),
                size: 3,
            }],
        };
        assert!(validate_release_files(&[file]).is_err());
    }

    #[test]
    fn profile_progress_moves_while_a_multi_chunk_file_downloads() {
        let chunks = [b"first chunk".as_slice(), b"second chunk".as_slice()];
        let chunk_refs = chunks
            .iter()
            .map(|chunk| ChunkRef {
                sha256: format!("{:x}", Sha256::digest(chunk)),
                size: chunk.len() as i64,
            })
            .collect::<Vec<_>>();
        let complete = chunks.concat();
        let listener = TcpListener::bind("127.0.0.1:0").unwrap();
        let address = listener.local_addr().unwrap();
        let served_chunks = chunk_refs.clone();
        let server = thread::spawn(move || {
            for _ in 0..served_chunks.len() {
                let (mut stream, _) = listener.accept().unwrap();
                let mut request = [0_u8; 4096];
                let read = stream.read(&mut request).unwrap();
                let request = String::from_utf8_lossy(&request[..read]);
                let payload = served_chunks
                    .iter()
                    .enumerate()
                    .find(|(_, chunk)| request.contains(&chunk.sha256))
                    .map(|(index, _)| chunks[index])
                    .unwrap();
                write!(
                    stream,
                    "HTTP/1.1 200 OK\r\nContent-Length: {}\r\nConnection: close\r\n\r\n",
                    payload.len()
                )
                .unwrap();
                stream.write_all(payload).unwrap();
            }
        });
        let root = test_root("progress");
        let files = root.join("files");
        let manifest = ProfileManifest {
            schema_version: SCHEMA_VERSION,
            kind: "profile".into(),
            release_id: "11111111-1111-1111-1111-111111111111".into(),
            sequence: 1,
            profile: ProfileConfig {
                id: "profile".into(),
                name: "Profile".into(),
                java_version: 21,
                jvm_args: String::new(),
                java_path_windows: String::new(),
                java_path_linux: String::new(),
                java_path_macos: String::new(),
                launch_command_windows: String::new(),
                launch_command_linux: String::new(),
                launch_command_macos: String::new(),
                preserve_paths: Vec::new(),
            },
            files: vec![ReleaseFile {
                path: "mods/large.jar".into(),
                size: complete.len() as i64,
                sha256: format!("{:x}", Sha256::digest(&complete)),
                executable: false,
                chunks: chunk_refs,
            }],
            file_count: 1,
            total_size: complete.len() as i64,
        };
        let samples = Arc::new(Mutex::new(Vec::new()));
        let progress_samples = Arc::clone(&samples);

        install_profile(
            &crate::download_client().unwrap(),
            &format!("http://{address}"),
            "token",
            &root,
            &files,
            &manifest,
            move |done, total| progress_samples.lock().unwrap().push((done, total)),
        )
        .unwrap();
        server.join().unwrap();

        let samples = samples.lock().unwrap();
        assert!(samples.len() >= 2, "progress samples = {samples:?}");
        assert!(samples.first().unwrap().0 < samples.first().unwrap().1);
        assert_eq!(samples.last().unwrap().0, samples.last().unwrap().1);
        let _ = fs::remove_dir_all(root);
    }

    #[test]
    fn profile_downloads_chunks_from_different_files_concurrently() {
        let payloads = [b"alpha".as_slice(), b"bravo".as_slice()];
        let chunks = payloads
            .iter()
            .map(|payload| ChunkRef {
                sha256: format!("{:x}", Sha256::digest(payload)),
                size: payload.len() as i64,
            })
            .collect::<Vec<_>>();
        let listener = TcpListener::bind("127.0.0.1:0").unwrap();
        let address = listener.local_addr().unwrap();
        let served_chunks = chunks.clone();
        let server = thread::spawn(move || {
            let mut requests = Vec::new();
            for _ in 0..2 {
                let (mut stream, _) = listener.accept().unwrap();
                let mut request = [0_u8; 4096];
                let read = stream.read(&mut request).unwrap();
                requests.push((
                    stream,
                    String::from_utf8_lossy(&request[..read]).to_string(),
                ));
            }
            for (mut stream, request) in requests {
                let payload = served_chunks
                    .iter()
                    .enumerate()
                    .find(|(_, chunk)| request.contains(&chunk.sha256))
                    .map(|(index, _)| payloads[index])
                    .unwrap();
                write!(
                    stream,
                    "HTTP/1.1 200 OK\r\nContent-Length: {}\r\nConnection: close\r\n\r\n",
                    payload.len()
                )
                .unwrap();
                stream.write_all(payload).unwrap();
            }
        });
        let root = test_root("parallel-files");
        let files = root.join("files");
        let release_files = chunks
            .into_iter()
            .enumerate()
            .map(|(index, chunk)| ReleaseFile {
                path: format!("mods/{index}.jar"),
                size: chunk.size,
                sha256: chunk.sha256.clone(),
                executable: false,
                chunks: vec![chunk],
            })
            .collect::<Vec<_>>();
        let manifest = ProfileManifest {
            schema_version: SCHEMA_VERSION,
            kind: "profile".into(),
            release_id: "22222222-2222-2222-2222-222222222222".into(),
            sequence: 1,
            profile: ProfileConfig {
                id: "profile".into(),
                name: "Profile".into(),
                java_version: 21,
                jvm_args: String::new(),
                java_path_windows: String::new(),
                java_path_linux: String::new(),
                java_path_macos: String::new(),
                launch_command_windows: String::new(),
                launch_command_linux: String::new(),
                launch_command_macos: String::new(),
                preserve_paths: Vec::new(),
            },
            file_count: release_files.len(),
            total_size: release_files.iter().map(|file| file.size).sum(),
            files: release_files,
        };
        let client = crate::download_client().unwrap();

        install_profile(
            &client,
            &format!("http://{address}"),
            "token",
            &root,
            &files,
            &manifest,
            |_, _| {},
        )
        .unwrap();
        server.join().unwrap();

        assert_eq!(fs::read(files.join("mods/0.jar")).unwrap(), b"alpha");
        assert_eq!(fs::read(files.join("mods/1.jar")).unwrap(), b"bravo");
        let _ = fs::remove_dir_all(root);
    }

    #[test]
    fn recovery_restores_backup_and_preserved_paths() {
        let root = test_root("recover");
        let files = root.join("files");
        let release = "11111111-1111-1111-1111-111111111111";
        let backup = root.join(format!(".delivery-backup-{release}"));
        let staging = root.join(format!(".delivery-staging-{release}"));
        fs::create_dir_all(backup.join("mods")).unwrap();
        fs::create_dir_all(staging.join("saves/world")).unwrap();
        fs::write(backup.join("mods/old.jar"), b"old").unwrap();
        fs::write(staging.join("saves/world/level.dat"), b"save").unwrap();
        let journal = SwapJournal {
            release_id: release.into(),
            phase: "preserving".into(),
            preserve_paths: vec!["saves/".into()],
        };
        fs::write(
            root.join("delivery-swap.json"),
            serde_json::to_vec(&journal).unwrap(),
        )
        .unwrap();

        recover_profile_swap(&root, &files).unwrap();

        assert_eq!(fs::read(files.join("mods/old.jar")).unwrap(), b"old");
        assert_eq!(
            fs::read(files.join("saves/world/level.dat")).unwrap(),
            b"save"
        );
        assert!(!staging.exists());
        assert!(!backup.exists());
        let _ = fs::remove_dir_all(root);
    }

    #[test]
    fn recovery_rejects_untrusted_release_path() {
        let root = test_root("untrusted");
        fs::create_dir_all(&root).unwrap();
        let journal = SwapJournal {
            release_id: "../outside".into(),
            phase: "prepared".into(),
            preserve_paths: Vec::new(),
        };
        fs::write(
            root.join("delivery-swap.json"),
            serde_json::to_vec(&journal).unwrap(),
        )
        .unwrap();
        assert!(recover_profile_swap(&root, &root.join("files")).is_err());
        let _ = fs::remove_dir_all(root);
    }
}
