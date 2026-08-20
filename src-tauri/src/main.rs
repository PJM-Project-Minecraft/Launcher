// Windows: release-сборка — GUI-приложение без консольного окна.
// В debug консоль оставляем для println-диагностики.
#![cfg_attr(all(windows, not(debug_assertions)), windows_subsystem = "windows")]

use std::collections::{HashMap, HashSet};
use std::fs::{self, File};
use std::io::{BufRead, BufReader, Read, Write};
use std::path::{Component, Path, PathBuf};
use std::process::{Command, ExitStatus, Stdio};
use std::sync::atomic::{AtomicBool, AtomicU64, AtomicUsize, Ordering};
use std::sync::{Arc, LazyLock, Mutex, OnceLock, RwLock};
use std::thread;
use std::time::{Duration, Instant, SystemTime, UNIX_EPOCH};

use base64::Engine as _;
use directories::ProjectDirs;
use rayon::prelude::*;
use reqwest::blocking::Client;
use serde::{Deserialize, Serialize};
use serde_json::Value;
use sha1::Sha1;
use sha2::{Digest, Sha256};
use tauri::Manager;

use ui_bridge::{invoke_from_ui, AppWindow, NewsItem, SharedString, UiState, Weak};

mod anticheat;
mod artifacts;
mod bundle;
mod discord_rpc;
mod gpu;
mod install;
mod ui_bridge;
mod updater;

const KEYRING_SERVICE: &str = "xyz.projectminecraft.launcher";
const KEYRING_USER: &str = "launcher-auth-token";
const JAVA_RUNTIME_INDEX_URL: &str =
    "https://piston-meta.mojang.com/v1/products/java-runtime/2ec0cc96c44e5a76b9c8b7c39df7210883d12871/all.json";
const DEFAULT_MEMORY_GB: i32 = 8;
const MIN_MEMORY_GB: i32 = 2;
const MAX_MEMORY_GB: i32 = 64;
static MANIFEST_CACHE_SEQUENCE: AtomicU64 = AtomicU64::new(0);

/// Зеркала бэкенда: игрок выбирает на окне входа, если основной домен недоступен.
/// Первым пунктом всегда идёт вшитый при сборке URL («Основной»), сюда — только
/// дополнительные адреса. Дубликат основного URL отсеивается в `api_mirrors`.
const EXTRA_API_MIRRORS: &[(&str, &str)] = &[("Зеркало", "https://mirror.likonchik.xyz")];

/// Таймаут пинга зеркала. Короткий: это подсказка в списке, а не проверка здоровья.
const MIRROR_PING_TIMEOUT: Duration = Duration::from_secs(5);

/// URL бэкенда меняется в рантайме (выбор зеркала на окне входа), а `AppConfig`
/// клонируется в замыкания и фоновые треды ещё на старте — поэтому ячейка общая.
#[derive(Clone)]
struct AppConfig {
    api_url: Arc<RwLock<String>>,
}

impl AppConfig {
    fn api_url(&self) -> String {
        self.api_url.read().unwrap().clone()
    }

    fn set_api_url(&self, url: &str) {
        *self.api_url.write().unwrap() = url.to_string();
    }
}

/// Список зеркал для окна входа: вшитый URL + `EXTRA_API_MIRRORS` без дублей.
fn api_mirrors(default_url: &str) -> Vec<(String, String)> {
    let mut mirrors = vec![("Основной".to_string(), default_url.to_string())];
    for (name, url) in EXTRA_API_MIRRORS {
        if *url != default_url {
            mirrors.push((name.to_string(), url.to_string()));
        }
    }
    mirrors
}

/// Подпись пункта «Авто» в списке серверов. Первым пунктом всегда идёт он: без
/// сохранённого выбора лаунчер сам берёт самый быстрый доступный сервер, а игрок с
/// режущимся провайдером не должен угадывать, какое зеркало ему подходит.
const AUTO_SERVER_LABEL: &str = "Авто — самый быстрый";

/// Пункты селектора сервера: «Авто» + зеркала. Индекс 0 — авто, дальше зеркала
/// в порядке `api_mirrors`.
fn server_items(mirrors: &[(String, String)]) -> Vec<String> {
    let mut items = vec![AUTO_SERVER_LABEL.to_string()];
    items.extend(mirrors.iter().map(|(name, _)| name.clone()));
    items
}

/// Индекс пункта селектора по сохранённому выбору: None (или мёртвый URL) → «Авто».
fn server_item_index(mirrors: &[(String, String)], saved: Option<&str>) -> usize {
    saved
        .and_then(|url| mirrors.iter().position(|(_, m)| m == url))
        .map(|i| i + 1)
        .unwrap_or(0)
}

/// Самый быстрый ДОСТУПНЫЙ сервер (None у недоступных отбрасываются). None — не
/// ответил никто: тогда остаёмся на текущем адресе, а не выключаем лаунчер.
fn best_ping_index(pings: &[Option<u128>]) -> Option<usize> {
    pings
        .iter()
        .enumerate()
        .filter_map(|(i, ms)| ms.map(|ms| (i, ms)))
        .min_by_key(|(_, ms)| *ms)
        .map(|(i, _)| i)
}

/// Доступные адреса от самого быстрого к самому медленному. При равной задержке
/// сохраняется исходный порядок, поэтому основной сервер имеет приоритет.
fn ranked_ping_indices(pings: &[Option<u128>]) -> Vec<usize> {
    let mut ranked = pings
        .iter()
        .enumerate()
        .filter_map(|(index, ping)| ping.map(|ms| (index, ms)))
        .collect::<Vec<_>>();
    ranked.sort_by_key(|(index, ms)| (*ms, *index));
    ranked.into_iter().map(|(index, _)| index).collect()
}

/// Пинг зеркала: время ответа публичного `/api/policy`. None — сеть/не-2xx, т.е.
/// зеркало непригодно (домен режется, прокси лежит).
///
/// Не `/health`: WAF-правило CF пропускает без челленджа только `/api/*`, а на
/// `/health` снаружи отдаёт 403-заглушку — основной сервер вечно «недоступен».
fn ping_mirror(url: &str) -> Option<u128> {
    let client = hardened_backend_builder()
        .timeout(MIRROR_PING_TIMEOUT)
        .build()
        .ok()?;
    let started = Instant::now();
    let response = client
        .get(format!("{}/api/policy", url.trim_end_matches('/')))
        .send()
        .ok()?;
    response
        .status()
        .is_success()
        .then(|| started.elapsed().as_millis())
}

fn mirror_label(name: &str, ping_ms: Option<u128>) -> String {
    match ping_ms {
        Some(ms) => format!("{name} — {ms} мс"),
        None => format!("{name} — недоступен"),
    }
}

fn probe_mirrors(mirrors: &[(String, String)]) -> Vec<Option<u128>> {
    thread::scope(|scope| {
        let handles = mirrors
            .iter()
            .map(|(_, url)| scope.spawn(move || ping_mirror(url)))
            .collect::<Vec<_>>();
        handles
            .into_iter()
            .map(|handle| handle.join().unwrap_or(None))
            .collect()
    })
}

fn update_mirror_labels(
    app_weak: &Weak<AppWindow>,
    mirrors: &[(String, String)],
    pings: &[Option<u128>],
) {
    for (index, ((name, _), ms)) in mirrors.iter().zip(pings.iter()).enumerate() {
        let label = mirror_label(name, *ms);
        let app_weak = app_weak.clone();
        let _ = app_weak.upgrade_in_event_loop(move |app| {
            app.set_server_name(index + 1, label);
        });
    }
}

/// Windows: прячет консольное окно дочернего консольного процесса (java, reg,
/// tasklist, cmd) — у GUI-приложения каждый такой запуск иначе открывает окно.
#[cfg(windows)]
pub(crate) fn hide_console_window(command: &mut Command) {
    use std::os::windows::process::CommandExt;
    const CREATE_NO_WINDOW: u32 = 0x0800_0000;
    command.creation_flags(CREATE_NO_WINDOW);
}

#[cfg(not(windows))]
pub(crate) fn hide_console_window(_command: &mut Command) {}

#[derive(Clone, Default)]
struct RuntimeState {
    token: String,
    user: Option<AuthUser>,
    profiles: Vec<ProfileSummary>,
    selected_profile_id: Option<String>,
}

#[derive(Clone, Copy, Debug, Eq, PartialEq)]
enum ProfileInstallState {
    Checking,
    Missing,
    UpdateAvailable,
    Ready,
    Unknown,
}

enum InitialInstallLocation {
    Current,
    Selected {
        source: PathBuf,
        destination: PathBuf,
    },
}

static SETTINGS_IO_LOCK: LazyLock<Mutex<()>> = LazyLock::new(|| Mutex::new(()));
static PROFILE_CHECK_SEQUENCE: AtomicU64 = AtomicU64::new(0);
static PROFILE_SYNC_ACTIVE: AtomicBool = AtomicBool::new(false);

/// Состояние автообновления. Глобальное (OnceLock): проверка живёт дольше
/// сессии логина и дёргается из SSE-слушателя, периодики и старта.
struct UpdateShared {
    /// Защита от параллельных скачиваний при множественных триггерах.
    in_progress: AtomicBool,
    /// Скачанное и проверенное обновление: (инфо, путь к staged-файлу).
    staged: Mutex<Option<(updater::UpdateInfo, PathBuf)>>,
}

static UPDATE_SHARED: OnceLock<Arc<UpdateShared>> = OnceLock::new();

fn update_shared() -> Arc<UpdateShared> {
    UPDATE_SHARED
        .get_or_init(|| {
            Arc::new(UpdateShared {
                in_progress: AtomicBool::new(false),
                staged: Mutex::new(None),
            })
        })
        .clone()
}

/// RAII-сброс флага in_progress: срабатывает и при панике в run_update_check,
/// иначе одна паника навсегда заглушила бы все будущие проверки обновлений.
struct InProgressGuard(Arc<UpdateShared>);

impl Drop for InProgressGuard {
    fn drop(&mut self) {
        self.0.in_progress.store(false, Ordering::SeqCst);
    }
}

/// Фоновая проверка обновления: запрос к бэкенду, скачивание и стейджинг.
/// Все UI-обновления — через invoke_from_event_loop. Повторный вызов во время
/// активной проверки игнорируется.
fn spawn_update_check(app_weak: Weak<AppWindow>, config: AppConfig) {
    let shared = update_shared();
    if shared.in_progress.swap(true, Ordering::SeqCst) {
        return;
    }
    thread::spawn(move || {
        let _guard = InProgressGuard(Arc::clone(&shared));
        run_update_check(&app_weak, &config, &shared);
    });
}

fn run_update_check(app_weak: &Weak<AppWindow>, config: &AppConfig, shared: &Arc<UpdateShared>) {
    let info = match updater::check_update(&config.api_url()) {
        Ok(info) => info,
        // Сервер недоступен — тихо ждём следующего триггера (старт/SSE/30 мин).
        Err(_) => return,
    };
    if !info.update_available {
        return;
    }
    // Клиентский guard от навязанного даунгрейда: не откатываемся на не-новее версию,
    // даже если сервер выставил update_available (сервер/зеркало могли быть скомпром.).
    if !updater::is_upgrade(&info.latest_version) {
        return;
    }

    // Уже скачали именно эту версию — только освежаем UI.
    let already_staged = shared
        .staged
        .lock()
        .ok()
        .and_then(|staged| {
            staged
                .as_ref()
                .map(|(stored, _)| stored.latest_version == info.latest_version)
        })
        .unwrap_or(false);
    if already_staged {
        set_update_ui(app_weak, &info, true, String::new());
        return;
    }

    set_update_ui(
        app_weak,
        &info,
        false,
        format!("Скачивается обновление {}…", info.latest_version),
    );

    match updater::download_and_stage(&config.api_url(), &info) {
        Ok(staged_path) => {
            if let Ok(mut staged) = shared.staged.lock() {
                *staged = Some((info.clone(), staged_path));
            }
            set_update_ui(app_weak, &info, true, String::new());
        }
        Err(message) => {
            // Ошибка остаётся в баннере; повтор — по следующему триггеру.
            set_update_ui(app_weak, &info, false, message);
        }
    }
}

/// Пробрасывает состояние обновления в UI-свойства (из любого потока).
fn set_update_ui(
    app_weak: &Weak<AppWindow>,
    info: &updater::UpdateInfo,
    ready: bool,
    status: String,
) {
    let app_weak = app_weak.clone();
    let version = info.latest_version.clone();
    let mandatory = info.mandatory;
    let _ = invoke_from_ui(move || {
        if let Some(app) = app_weak.upgrade() {
            app.set_update_ready(ready);
            app.set_update_mandatory(mandatory);
            app.set_update_version(version.into());
            app.set_update_status(status.into());
        }
    });
}

/// Колбэк «Перезапустить»: подменяет бинарник и перезапускает процесс.
fn register_update_restart_handler(app: &AppWindow) {
    let app_weak = app.as_weak();
    app.on_update_restart_requested(move || {
        let shared = update_shared();
        let staged = shared.staged.lock().ok().and_then(|staged| staged.clone());
        let Some((info, staged_path)) = staged else {
            return;
        };
        if let Err(message) = updater::apply_and_restart(&staged_path, &info) {
            // Подмена не удалась: сбрасываем staged (файл мог быть повреждён).
            if let Ok(mut staged) = shared.staged.lock() {
                *staged = None;
            }
            if let Some(app) = app_weak.upgrade() {
                app.set_update_ready(false);
                app.set_update_mandatory(info.mandatory);
                app.set_update_status(message.into());
            }
        }
    });
}

/// Страховочный фоновый опрос обновлений раз в 30 минут (SSE — основной канал).
fn start_periodic_update_check(app_weak: Weak<AppWindow>, config: AppConfig) {
    thread::spawn(move || loop {
        thread::sleep(Duration::from_secs(30 * 60));
        spawn_update_check(app_weak.clone(), config.clone());
    });
}

#[derive(Debug, Serialize)]
#[serde(rename_all = "camelCase")]
struct LoginRequest {
    login: String,
    password: String,
    totp: Option<String>,
}

#[derive(Debug, Deserialize)]
#[serde(rename_all = "camelCase")]
struct LoginResponse {
    token: String,
    expires_at: String,
    user: AuthUser,
    message: String,
}

#[derive(Debug, Deserialize, Clone)]
#[serde(rename_all = "camelCase")]
struct AuthUser {
    #[serde(default)]
    id: String,
    login: String,
    provider_uuid: String,
    is_slim: bool,
    #[serde(default)]
    policy_accepted_version: i32,
}

#[derive(Debug, Deserialize, Clone)]
#[serde(rename_all = "camelCase")]
struct PolicyInfo {
    version: i32,
    text: String,
}

#[derive(Debug, Deserialize, Clone)]
#[serde(rename_all = "camelCase")]
struct ProfileSummary {
    id: String,
    name: String,
    game_version: String,
    #[serde(default)]
    is_active: bool,
}

#[derive(Debug, Deserialize, Clone)]
#[serde(rename_all = "camelCase")]
struct NewsSummary {
    #[serde(default)]
    title: String,
    #[serde(default)]
    body: String,
    #[serde(default)]
    created_at: String,
}

#[derive(Debug, Deserialize, Clone)]
#[serde(rename_all = "camelCase")]
struct Manifest {
    profile: ManifestProfile,
    files: Vec<ManifestFile>,
    #[serde(default)]
    preserve_paths: Vec<String>,
    file_count: usize,
    total_size: i64,
    #[serde(default)]
    bundle: Option<bundle::BundleInfo>,
}

#[derive(Debug, Deserialize, Clone)]
#[serde(rename_all = "camelCase")]
struct ManifestProfile {
    id: String,
    name: String,
    #[serde(default)]
    java_version: i32,
    #[serde(default)]
    jvm_args: String,
    #[serde(default)]
    java_path_windows: String,
    #[serde(default)]
    java_path_linux: String,
    #[serde(default)]
    java_path_macos: String,
    #[serde(default)]
    launch_command_windows: String,
    #[serde(default)]
    launch_command_linux: String,
    #[serde(default)]
    launch_command_macos: String,
    manifest_version: i32,
}

#[derive(Debug, Deserialize, Serialize, Clone)]
#[serde(rename_all = "camelCase")]
struct ManifestFile {
    id: String,
    name: String,
    path: String,
    download_url: String,
    hash_sha256: String,
    size: i64,
    file_type: String,
    #[serde(default)]
    executable: bool,
}

type JavaRuntimeIndex = HashMap<String, HashMap<String, Vec<JavaRuntimeRelease>>>;

#[derive(Debug, Deserialize)]
struct JavaRuntimeRelease {
    manifest: JavaRuntimeManifestRef,
}

#[derive(Debug, Deserialize)]
struct JavaRuntimeManifestRef {
    url: String,
    sha1: String,
    size: i64,
}

#[derive(Debug, Deserialize)]
struct JavaRuntimeManifest {
    files: HashMap<String, JavaRuntimeFile>,
}

#[derive(Debug, Deserialize)]
struct JavaRuntimeFile {
    #[serde(rename = "type")]
    kind: String,
    #[serde(default)]
    executable: bool,
    #[serde(default)]
    target: String,
    downloads: Option<JavaRuntimeDownloads>,
}

#[derive(Debug, Deserialize)]
struct JavaRuntimeDownloads {
    raw: Option<JavaRuntimeDownload>,
}

#[derive(Debug, Deserialize, Clone)]
struct JavaRuntimeDownload {
    url: String,
    sha1: String,
    size: i64,
}

struct JavaRuntimeDownloadTask {
    path: String,
    download: JavaRuntimeDownload,
    executable: bool,
}

#[derive(Debug, Deserialize)]
#[serde(rename_all = "camelCase")]
struct ErrorResponse {
    message: Option<String>,
    requires_two_factor: Option<bool>,
}

#[derive(Debug)]
struct LoginError {
    message: String,
    requires_two_factor: bool,
}

#[derive(Debug, Serialize, Deserialize, Clone)]
#[serde(rename_all = "camelCase")]
struct LauncherSettings {
    #[serde(default)]
    auth_token: Option<String>,
    #[serde(default)]
    last_user_uuid: Option<String>,
    #[serde(default)]
    selected_profiles: HashMap<String, String>,
    #[serde(default = "default_memory_gb")]
    memory_gb: i32,
    #[serde(default = "default_memory_auto")]
    memory_auto: bool,
    #[serde(default = "default_use_discrete_gpu")]
    use_discrete_gpu: bool,
    #[serde(default = "default_discord_rpc_enabled")]
    discord_rpc_enabled: bool,
    /// Корень тяжёлых файлов игры (`users/<uuid>/profiles/...`). None сохраняет
    /// совместимость со старым системным data-dir.
    #[serde(default)]
    install_root: Option<String>,
    /// Выбранное на окне входа зеркало бэкенда. None → «Основной».
    #[serde(default)]
    api_url: Option<String>,
    /// Суммарное наигранное время (секунды), локальный счётчик этой машины.
    #[serde(default)]
    played_seconds: u64,
}

impl Default for LauncherSettings {
    fn default() -> Self {
        Self {
            auth_token: None,
            last_user_uuid: None,
            selected_profiles: HashMap::new(),
            memory_gb: DEFAULT_MEMORY_GB,
            memory_auto: true,
            use_discrete_gpu: true,
            discord_rpc_enabled: true,
            install_root: None,
            api_url: None,
            played_seconds: 0,
        }
    }
}

#[derive(Debug, Serialize, Deserialize, Default)]
#[serde(rename_all = "camelCase")]
struct LocalManifest {
    profile_id: String,
    manifest_version: i32,
    files: Vec<LocalFileRecord>,
}

#[derive(Debug, Serialize, Deserialize, Clone)]
#[serde(rename_all = "camelCase")]
struct LocalFileRecord {
    path: String,
    hash_sha256: String,
    size: i64,
    #[serde(default)]
    mtime_millis: i64,
}

struct SessionData {
    token: String,
    user: AuthUser,
    expires_at: String,
    message: String,
    profiles: Vec<ProfileSummary>,
    selected_profile_id: Option<String>,
    news: Vec<NewsSummary>,
    policy: Option<PolicyInfo>,
}

struct ProfilePaths {
    profile_root: PathBuf,
    files_root: PathBuf,
    manifest_path: PathBuf,
}

fn main() {
    tauri::Builder::default()
        .setup(|tauri_app| {
            let app = AppWindow::new(tauri_app.handle().clone());
            initialize_launcher(&app);
            tauri_app.manage(app);
            Ok(())
        })
        .invoke_handler(tauri::generate_handler![
            get_launcher_state,
            launcher_action
        ])
        .run(tauri::generate_context!())
        .expect("не удалось запустить Project Minecraft Launcher");
}

#[tauri::command]
fn get_launcher_state(app: tauri::State<'_, AppWindow>) -> UiState {
    app.snapshot()
}

#[tauri::command]
fn launcher_action(
    action: String,
    payload: Option<Value>,
    app: tauri::State<'_, AppWindow>,
) -> Result<(), String> {
    app.dispatch(&action, payload.unwrap_or(Value::Null))
}

fn initialize_launcher(app: &AppWindow) {
    let default_api_url = option_env!("LAUNCHER_DEFAULT_API_URL")
        .unwrap_or("http://127.0.0.1:8080")
        .to_string();
    // Зеркала: env LAUNCHER_API_URL перебивает вшитый URL и становится «Основным».
    // Выбор игрока (settings.json) применяется, только если такое зеркало ещё есть.
    let mirrors = api_mirrors(&std::env::var("LAUNCHER_API_URL").unwrap_or(default_api_url));
    let saved_mirror = load_settings().unwrap_or_default().api_url;
    let server_idx = server_item_index(&mirrors, saved_mirror.as_deref());
    // В авто-режиме (нет сохранённого выбора) стартуем с основного адреса, а фоновый
    // пинг переключит на самый быстрый: ждать результата пинга перед показом окна
    // входа нельзя — мёртвое зеркало задержало бы запуск на таймаут.
    let start_url = mirrors[server_idx.saturating_sub(1)].1.clone();
    let config = AppConfig {
        api_url: Arc::new(RwLock::new(start_url)),
    };

    // Discord Rich Presence (опционально). Client ID — из env при сборке или
    // константы-плейсхолдера; при "0" rpc_init — no-op.
    let discord_client_id =
        option_env!("DISCORD_CLIENT_ID").unwrap_or(discord_rpc::DEFAULT_DISCORD_CLIENT_ID);
    discord_rpc::rpc_init(discord_client_id);

    // Автообновление: подчищаем следы прошлой установки и проверяем новую
    // версию при старте (до логина — эндпоинт публичный).
    updater::cleanup_leftovers();
    app.set_api_url(config.api_url().into());
    let saved_token = read_token().ok().filter(|token| !token.trim().is_empty());
    let restoring_through_auto = saved_token.is_some() && server_idx == 0;
    register_mirror_handler(
        app,
        config.clone(),
        mirrors.clone(),
        server_idx,
        !restoring_through_auto,
    );
    app.set_message("Готов к входу.".into());
    app.set_profile_status("Offline".into());
    app.set_selected_profile_name(SharedString::default());
    app.set_selected_profile_version("-".into());
    app.set_download_phase(SharedString::default());
    app.set_download_file(SharedString::default());
    app.set_download_counter(SharedString::default());
    app.set_download_progress(0.0);
    app.set_download_panel_visible(false);

    let state = Arc::new(Mutex::new(RuntimeState::default()));
    // Поколение сессии: при логине/перелогине увеличивается, что останавливает
    // фоновый SSE-слушатель предыдущей сессии (см. start_profile_event_listener).
    let session_generation = Arc::new(AtomicU64::new(0));
    apply_launcher_settings(app, &load_settings().unwrap_or_default());
    {
        let settings = load_settings().unwrap_or_default();
        discord_rpc::rpc_set_enabled(settings.discord_rpc_enabled);
        discord_rpc::rpc_set(discord_rpc::Presence::Idle);
    }
    apply_install_folder_label(app);

    register_login_handler(
        app,
        config.clone(),
        state.clone(),
        session_generation.clone(),
    );
    register_policy_accept_handler(app, config.clone(), state.clone());
    let migration_active = Arc::new(AtomicBool::new(false));
    register_logout_handler(
        app,
        state.clone(),
        session_generation.clone(),
        migration_active.clone(),
        config.clone(),
        mirrors.clone(),
    );
    register_settings_handler(app, state.clone(), migration_active.clone());
    register_play_handler(app, config.clone(), state.clone(), migration_active);
    register_update_restart_handler(app);
    spawn_update_check(app.as_weak(), config.clone());
    start_periodic_update_check(app.as_weak(), config.clone());
    restore_saved_session(
        app,
        config,
        state,
        session_generation,
        saved_token,
        mirrors,
        server_idx,
    );
}

fn register_login_handler(
    app: &AppWindow,
    config: AppConfig,
    state: Arc<Mutex<RuntimeState>>,
    generation: Arc<AtomicU64>,
) {
    let login_app = app.as_weak();
    app.on_login_requested(move |login, password, totp| {
        let login = login.to_string();
        let password = password.to_string();
        let totp = normalize_totp_code(totp.as_str());
        let submitted_totp = !totp.is_empty();

        if login.trim().is_empty() || password.is_empty() {
            if let Some(app) = login_app.upgrade() {
                app.set_message("Введите логин и пароль.".into());
            }
            return;
        }

        if let Some(app) = login_app.upgrade() {
            app.set_is_loading(true);
            app.set_message("Проверяем аккаунт...".into());
        }
        discord_rpc::rpc_set(discord_rpc::Presence::LoggingIn);

        let app_weak = login_app.clone();
        let config = config.clone();
        let state = state.clone();
        let generation = generation.clone();
        thread::spawn(move || {
            let result = login_and_bootstrap(&config, login, password, totp);
            let _ = invoke_from_ui(move || {
                if let Some(app) = app_weak.upgrade() {
                    app.set_is_loading(false);
                    match result {
                        Ok(session) => apply_session(&app, &state, &config, &generation, session),
                        Err(error) => {
                            // Вход не удался — возвращаем presence в исходное состояние.
                            discord_rpc::rpc_set(discord_rpc::Presence::Idle);
                            let keep_totp_prompt = error.requires_two_factor || submitted_totp;
                            app.set_requires_totp(keep_totp_prompt);
                            if keep_totp_prompt {
                                app.set_totp_value(SharedString::default());
                            }
                            if error.requires_two_factor && !submitted_totp {
                                app.set_message(SharedString::default());
                            } else {
                                app.set_message(error.message.into());
                            }
                        }
                    }
                }
            });
        });
    });
}

fn register_policy_accept_handler(
    app: &AppWindow,
    config: AppConfig,
    state: Arc<Mutex<RuntimeState>>,
) {
    let app_weak = app.as_weak();
    let state = state.clone();
    app.on_policy_accept_requested(move || {
        let Some(app) = app_weak.upgrade() else {
            return;
        };
        // Защита от двойного клика: игнорируем повторный вызов пока запрос выполняется.
        if app.get_policy_accepting() {
            return;
        }
        app.set_policy_accepting(true);
        let token = state
            .lock()
            .ok()
            .map(|s| s.token.clone())
            .unwrap_or_default();
        let version = app.get_policy_version();
        app.set_message("Сохраняю согласие…".into());
        let config = config.clone();
        let app_weak = app_weak.clone();
        thread::spawn(move || {
            let result = accept_policy(&config, &token, version);
            let refreshed = if result.is_err() {
                // Возможен 409: версия сменилась — перечитываем текст.
                fetch_policy(&config).ok()
            } else {
                None
            };
            let _ = invoke_from_ui(move || {
                let Some(app) = app_weak.upgrade() else {
                    return;
                };
                match result {
                    Ok(()) => {
                        app.set_policy_accepting(false);
                        app.set_policy_visible(false);
                        app.set_message("Политика принята. Приятной игры!".into());
                    }
                    Err(message) => {
                        app.set_policy_accepting(false);
                        if let Some(p) = refreshed {
                            app.set_policy_text(p.text.into());
                            app.set_policy_version_label(format!("Версия {}", p.version).into());
                            app.set_policy_version(p.version);
                        }
                        app.set_message(message.into());
                    }
                }
            });
        });
    });
}

fn register_logout_handler(
    app: &AppWindow,
    state: Arc<Mutex<RuntimeState>>,
    generation: Arc<AtomicU64>,
    migration_active: Arc<AtomicBool>,
    config: AppConfig,
    mirrors: Vec<(String, String)>,
) {
    let logout_app = app.as_weak();
    app.on_logout_requested(move || {
        if migration_active.load(Ordering::SeqCst) {
            if let Some(app) = logout_app.upgrade() {
                app.set_message("Дождитесь завершения переноса файлов перед выходом.".into());
            }
            return;
        }
        let _ = delete_token();
        discord_rpc::rpc_set(discord_rpc::Presence::Idle);
        // Останавливаем фоновый SSE-слушатель текущей сессии.
        generation.fetch_add(1, Ordering::SeqCst);
        if let Ok(mut state) = state.lock() {
            *state = RuntimeState::default();
        }
        if let Some(app) = logout_app.upgrade() {
            app.set_is_authenticated(false);
            app.set_requires_totp(false);
            app.set_user_login(SharedString::default());
            app.set_user_uuid(SharedString::default());
            app.set_token_expires_at(SharedString::default());
            app.set_login_value(SharedString::default());
            app.set_password_value(SharedString::default());
            app.set_totp_value(SharedString::default());
            app.set_is_slim(false);
            app.set_has_profile(false);
            app.set_profile_status("Offline".into());
            app.set_selected_profile_name(SharedString::default());
            app.set_selected_profile_version("-".into());
            app.set_is_syncing(false);
            app.set_download_panel_visible(false);
            app.set_settings_visible(false);
            app.set_policy_visible(false);
            apply_install_folder_label(&app);
            app.set_message("Сессия завершена.".into());

            // Выбор сервера, сделанный в настройках во время живой игровой сессии,
            // применяется только теперь: до logout менять backend нельзя, потому что
            // yggdrasil-сессия и JWT принадлежат адресу, на котором начался вход.
            let selected = load_settings().unwrap_or_default().api_url;
            let index = server_item_index(&mirrors, selected.as_deref());
            app.set_server_index(index as i32);
            if index == 0 {
                if let Some((_, url)) = mirrors.first() {
                    config.set_api_url(url);
                    app.set_api_url(url.clone());
                }
                spawn_mirror_probe(app.as_weak(), config.clone(), mirrors.clone(), true);
            } else if let Some((name, url)) = mirrors.get(index - 1) {
                config.set_api_url(url);
                app.set_api_url(url.clone());
                app.set_message(format!("Сессия завершена. Сервер: {name}").into());
            }
        }
    });
}

fn register_settings_handler(
    app: &AppWindow,
    state: Arc<Mutex<RuntimeState>>,
    migration_active: Arc<AtomicBool>,
) {
    let settings_app = app.as_weak();
    app.on_settings_requested(move || {
        if let Some(app) = settings_app.upgrade() {
            apply_launcher_settings(&app, &load_settings().unwrap_or_default());
            apply_install_folder_label(&app);
            app.set_settings_visible(true);
        }
    });

    let close_app = app.as_weak();
    app.on_settings_close_requested(move || {
        if let Some(app) = close_app.upgrade() {
            app.set_settings_visible(false);
        }
    });

    let auto_app = app.as_weak();
    app.on_memory_auto_requested(move || {
        update_memory_settings(&auto_app, |settings| {
            settings.memory_auto = true;
            settings.memory_gb = DEFAULT_MEMORY_GB;
        });
    });

    let decrease_app = app.as_weak();
    app.on_memory_decrease_requested(move || {
        update_memory_settings(&decrease_app, |settings| {
            let current = effective_memory_gb(settings);
            settings.memory_auto = false;
            settings.memory_gb = clamp_memory_gb(current - 1);
        });
    });

    let increase_app = app.as_weak();
    app.on_memory_increase_requested(move || {
        update_memory_settings(&increase_app, |settings| {
            let current = effective_memory_gb(settings);
            settings.memory_auto = false;
            settings.memory_gb = clamp_memory_gb(current + 1);
        });
    });

    // Перетаскивание слайдера: UI присылает целевое значение в ГБ.
    let slider_app = app.as_weak();
    app.on_memory_set_requested(move |value| {
        update_memory_settings(&slider_app, move |settings| {
            settings.memory_auto = false;
            settings.memory_gb = clamp_memory_gb(value);
        });
    });

    let gpu_app = app.as_weak();
    app.on_discrete_gpu_requested(move |enabled| {
        if let Some(app) = gpu_app.upgrade() {
            match update_settings(|settings| settings.use_discrete_gpu = enabled) {
                Ok(settings) => {
                    apply_launcher_settings(&app, &settings);
                    app.set_message(
                        if enabled {
                            "Игра будет запускаться на дискретной видеокарте."
                        } else {
                            "Игра будет запускаться на встроенной видеокарте."
                        }
                        .into(),
                    );
                }
                Err(message) => app.set_message(message.into()),
            }
        }
    });

    let discord_app = app.as_weak();
    app.on_discord_rpc_requested(move |enabled| {
        if let Some(app) = discord_app.upgrade() {
            match update_settings(|settings| settings.discord_rpc_enabled = enabled) {
                Ok(settings) => {
                    apply_launcher_settings(&app, &settings);
                    discord_rpc::rpc_set_enabled(enabled);
                    app.set_message(
                        if enabled {
                            "Discord Rich Presence включён."
                        } else {
                            "Discord Rich Presence выключён."
                        }
                        .into(),
                    );
                }
                Err(message) => app.set_message(message.into()),
            }
        }
    });

    // «Папка модов» в сайдбаре: mods внутри files_root выбранного профиля.
    let mods_app = app.as_weak();
    let mods_state = state.clone();
    app.on_open_mods_folder_requested(move || {
        if let Some(app) = mods_app.upgrade() {
            let folder = mods_state
                .lock()
                .map_err(|_| "Не удалось прочитать состояние лаунчера.".to_string())
                .and_then(|state| install_folder_for_state(&state).map(|root| root.join("mods")));

            match folder {
                Ok(path) => {
                    if let Err(message) = fs::create_dir_all(&path)
                        .map_err(|_| "Не удалось создать папку модов.".to_string())
                        .and_then(|_| open_folder(&path))
                    {
                        app.set_message(message.into());
                        return;
                    }
                    app.set_message("Папка модов открыта.".into());
                }
                Err(message) => app.set_message(message.into()),
            }
        }
    });

    let folder_app = app.as_weak();
    app.on_open_install_folder_requested(move || {
        if let Some(app) = folder_app.upgrade() {
            let folder = current_install_root();

            match folder {
                Ok(path) => {
                    if let Err(message) = fs::create_dir_all(&path)
                        .map_err(|_| "Не удалось создать папку установки.".to_string())
                        .and_then(|_| open_folder(&path))
                    {
                        app.set_message(message.into());
                        return;
                    }
                    app.set_install_folder(path.to_string_lossy().to_string().into());
                    app.set_message("Папка установки открыта.".into());
                }
                Err(message) => app.set_message(message.into()),
            }
        }
    });

    let change_folder_app = app.as_weak();
    let change_folder_active = migration_active.clone();
    app.on_change_install_folder_requested(move || {
        let Some(app) = change_folder_app.upgrade() else {
            return;
        };
        if app.get_is_syncing() {
            app.set_message("Дождитесь завершения установки или игры перед сменой диска.".into());
            return;
        }
        if change_folder_active
            .compare_exchange(false, true, Ordering::SeqCst, Ordering::SeqCst)
            .is_err()
        {
            app.set_message("Перенос файлов уже выполняется.".into());
            return;
        }

        let source_root = match current_install_root() {
            Ok(path) => path,
            Err(message) => {
                change_folder_active.store(false, Ordering::SeqCst);
                app.set_message(message.into());
                return;
            }
        };
        let selected = rfd::FileDialog::new()
            .set_title("Выберите пустую папку на новом диске")
            .set_directory(&source_root)
            .pick_folder();
        let Some(destination) = selected else {
            change_folder_active.store(false, Ordering::SeqCst);
            return;
        };

        app.set_is_syncing(true);
        app.set_message("Переносим профили на новый диск. Не закрывайте лаунчер…".into());
        PROFILE_SYNC_ACTIVE.store(true, Ordering::SeqCst);
        let app_weak = change_folder_app.clone();
        let active = change_folder_active.clone();
        thread::spawn(move || {
            let result = install::migrate_users(&source_root, &destination).and_then(|migration| {
                update_settings(|settings| {
                    settings.install_root =
                        Some(migration.destination.to_string_lossy().to_string());
                })?;
                install::finalize_migration(&migration);
                Ok(migration)
            });
            PROFILE_SYNC_ACTIVE.store(false, Ordering::SeqCst);
            active.store(false, Ordering::SeqCst);

            let _ = invoke_from_ui(move || {
                if let Some(app) = app_weak.upgrade() {
                    app.set_is_syncing(false);
                    match result {
                        Ok(migration) => {
                            apply_install_folder_label(&app);
                            let message = if migration.copied_existing_data {
                                format!(
                                    "Установка перенесена в {}. Старая папка оставлена как резервная копия.",
                                    migration.destination.display()
                                )
                            } else {
                                format!("Новая папка установки: {}", migration.destination.display())
                            };
                            app.set_message(message.into());
                        }
                        Err(message) => app.set_message(message.into()),
                    }
                }
            });
        });
    });

    app.on_open_url(|url| {
        let url = url.to_string();
        let mut command = if cfg!(target_os = "windows") {
            // rundll32 FileProtocolHandler открывает URL в браузере, передавая его ЕДИНЫМ
            // аргументом — без разбора шелла (в отличие от `cmd /C start`, где & | ^ в URL
            // стали бы инъекцией команды, попади в url когда-нибудь строка из сети).
            let mut cmd = Command::new("rundll32.exe");
            cmd.args(["url.dll,FileProtocolHandler", &url]);
            cmd
        } else if cfg!(target_os = "macos") {
            let mut cmd = Command::new("open");
            cmd.arg(&url);
            cmd
        } else {
            let mut cmd = Command::new("xdg-open");
            cmd.arg(&url);
            cmd
        };
        hide_console_window(&mut command);
        let _ = command.spawn();
    });

    // Закрытие полноэкранного уведомления античита.
    let dismiss_app = app.as_weak();
    app.on_anticheat_alert_dismiss(move || {
        if let Some(app) = dismiss_app.upgrade() {
            app.set_anticheat_alert(SharedString::default());
        }
    });
}

fn register_play_handler(
    app: &AppWindow,
    config: AppConfig,
    state: Arc<Mutex<RuntimeState>>,
    migration_active: Arc<AtomicBool>,
) {
    let play_app = app.as_weak();
    app.on_play_requested(move || {
        let snapshot = match state.lock() {
            Ok(state) => state.clone(),
            Err(_) => {
                if let Some(app) = play_app.upgrade() {
                    app.set_message("Не удалось прочитать состояние лаунчера.".into());
                }
                return;
            }
        };

        let token = snapshot.token.clone();
        let user = match snapshot.user.clone() {
            Some(user) => user,
            None => {
                if let Some(app) = play_app.upgrade() {
                    app.set_message("Сначала войдите в аккаунт.".into());
                }
                return;
            }
        };
        let profile = match selected_profile(&snapshot) {
            Some(profile) => profile,
            None => {
                if let Some(app) = play_app.upgrade() {
                    app.set_message("Активные профили проекта не найдены.".into());
                }
                return;
            }
        };

        let initial_location = match choose_initial_install_root(&user, &profile) {
            Ok(Some(location)) => location,
            Ok(None) => {
                if let Some(app) = play_app.upgrade() {
                    app.set_message("Установка отменена: папка не выбрана.".into());
                }
                return;
            }
            Err(message) => {
                if let Some(app) = play_app.upgrade() {
                    app.set_message(message.into());
                }
                return;
            }
        };
        let is_initial_migration = matches!(initial_location, InitialInstallLocation::Selected { .. });
        if is_initial_migration
            && migration_active
                .compare_exchange(false, true, Ordering::SeqCst, Ordering::SeqCst)
                .is_err()
        {
            if let Some(app) = play_app.upgrade() {
                app.set_message("Перенос файлов уже выполняется.".into());
            }
            return;
        }

        if let Some(app) = play_app.upgrade() {
            PROFILE_SYNC_ACTIVE.store(true, Ordering::SeqCst);
            app.set_is_syncing(true);
            app.set_settings_visible(false);
            app.set_download_panel_visible(true);
            app.set_download_phase(
                if is_initial_migration { "Готовим папку" } else { "Получаем профиль" }.into(),
            );
            app.set_download_file(profile.name.clone().into());
            app.set_download_counter("0%".into());
            app.set_download_progress(0.0);
            app.set_message("Готовим профиль к запуску...".into());
        }
        discord_rpc::rpc_set(discord_rpc::Presence::Downloading {
            nick: user.login.clone(),
        });

        let nick_for_rpc = user.login.clone();
        let app_weak = play_app.clone();
        let config = config.clone();
        let migration_flag = migration_active.clone();
        let refresh_runtime = state.clone();
        thread::spawn(move || {
            let preparation = match initial_location {
                InitialInstallLocation::Current => Ok(()),
                InitialInstallLocation::Selected { source, destination } => {
                    install::migrate_users(&source, &destination).and_then(|migration| {
                        update_settings(|settings| {
                            settings.install_root =
                                Some(migration.destination.to_string_lossy().to_string());
                        })?;
                        install::finalize_migration(&migration);
                        Ok(())
                    })
                }
            };
            if is_initial_migration {
                migration_flag.store(false, Ordering::SeqCst);
            }
            let result = preparation
                .and_then(|_| sync_and_launch(&config, &token, &user, &profile, &app_weak));
            PROFILE_SYNC_ACTIVE.store(false, Ordering::SeqCst);
            if let Err(ref message) = result {
                log_sync_error(message);
            }
            let nick_for_rpc = nick_for_rpc.clone();
            let refresh_app = app_weak.clone();
            let refresh_config = config.clone();
            let refresh_token = token.clone();
            let refresh_user = user.clone();
            let refresh_profile = profile.clone();
            let _ = invoke_from_ui(move || {
                if let Some(app) = app_weak.upgrade() {
                    apply_install_folder_label(&app);
                    discord_rpc::rpc_set(discord_rpc::Presence::Browsing {
                        nick: nick_for_rpc.clone(),
                    });
                    let _ = app.show();
                    app.set_is_syncing(false);
                    match result {
                        Ok(message) => {
                            if !app.get_profile_update_available() {
                                set_profile_install_state(&app, ProfileInstallState::Checking);
                            }
                            app.set_download_phase("Готово".into());
                            app.set_download_file("Minecraft закрыт".into());
                            app.set_download_counter("100%".into());
                            app.set_download_progress(1.0);
                            app.set_download_panel_visible(false);
                            app.set_message(message.into());
                            let played = load_settings().unwrap_or_default().played_seconds;
                            app.set_playtime_total(format_playtime(played).into());
                            refresh_profile_install_state(
                                refresh_app,
                                refresh_runtime,
                                refresh_config,
                                refresh_token,
                                refresh_user,
                                refresh_profile,
                            );
                        }
                        Err(message) => {
                            if let Some(alert) = message.strip_prefix(anticheat::kick::KICK_PREFIX) {
                                // Игру закрыл античит — полноэкранное уведомление.
                                app.set_download_panel_visible(false);
                                app.set_message(SharedString::default());
                                app.set_anticheat_alert(alert.into());
                            } else if message == anticheat::POLICY_REQUIRED_ERR {
                                // Редкий случай (окно раскатки): сервер требует согласие —
                                // показываем экран политики вместо сырого текста ошибки.
                                app.set_download_panel_visible(false);
                                let config_bg = config.clone();
                                let app_weak_bg = app_weak.clone();
                                thread::spawn(move || {
                                    let policy = fetch_policy(&config_bg);
                                    let _ = invoke_from_ui(move || {
                                        let Some(app) = app_weak_bg.upgrade() else { return };
                                        if let Ok(p) = policy {
                                            app.set_policy_text(p.text.into());
                                            app.set_policy_version_label(
                                                format!("Версия {}", p.version).into(),
                                            );
                                            app.set_policy_version(p.version);
                                        } else if app.get_policy_text().is_empty() {
                                            // Fallback: текст не загрузился — не оставляем оверлей пустым.
                                            app.set_policy_text(
                                                "Не удалось загрузить текст политики. Проверьте соединение и попробуйте снова.".into(),
                                            );
                                        }
                                        app.set_policy_visible(true);
                                    });
                                });
                            } else {
                                app.set_download_phase("Ошибка".into());
                                // На карточке одна строка — берём первую строку сообщения;
                                // детали (хвост лога) остаются в sync-errors.log.
                                let first_line = message
                                    .lines()
                                    .next()
                                    .unwrap_or("Неизвестная ошибка")
                                    .to_string();
                                app.set_download_file(first_line.into());
                                app.set_download_panel_visible(true);
                                app.set_message(message.into());
                            }
                        }
                    }
                }
            });
        });
    });
}

/// На старых установках `installRoot` отсутствует. Если у выбранного профиля уже
/// есть локальный manifest, продолжаем использовать прежний data-dir. Если файлов
/// ещё нет, перед первой загрузкой обязательно предлагаем место установки.
fn choose_initial_install_root(
    user: &AuthUser,
    profile: &ProfileSummary,
) -> Result<Option<InitialInstallLocation>, String> {
    let settings = load_settings()?;
    let default_root = project_dirs()?.data_dir().to_path_buf();
    let configured_root = settings
        .install_root
        .as_deref()
        .map(str::trim)
        .filter(|root| !root.is_empty())
        .map(|root| install::configured_root(Some(root), &default_root))
        .transpose()?;
    if configured_root.as_ref().is_some_and(|root| root.is_dir()) {
        return Ok(Some(InitialInstallLocation::Current));
    }
    if configured_root.is_none()
        && profile_paths_at_root(user, &profile.id, &default_root)?
            .manifest_path
            .is_file()
    {
        return Ok(Some(InitialInstallLocation::Current));
    }

    let source = configured_root.unwrap_or(default_root);

    let selected = rfd::FileDialog::new()
        .set_title("Куда установить файлы Minecraft?")
        .set_directory(source.parent().unwrap_or(&source))
        .pick_folder();
    let Some(selected) = selected else {
        return Ok(None);
    };
    Ok(Some(InitialInstallLocation::Selected {
        source,
        destination: selected,
    }))
}

fn restore_saved_session(
    app: &AppWindow,
    config: AppConfig,
    state: Arc<Mutex<RuntimeState>>,
    generation: Arc<AtomicU64>,
    token: Option<String>,
    mirrors: Vec<(String, String)>,
    server_idx: usize,
) {
    let Some(token) = token else {
        app.set_is_loading(false);
        app.set_session_restoring(false);
        return;
    };

    app.set_session_restoring(true);
    app.set_is_loading(true);
    app.set_message("Восстанавливаем сохранённую сессию…".into());
    discord_rpc::rpc_set(discord_rpc::Presence::LoggingIn);

    let app_weak = app.as_weak();
    thread::spawn(move || {
        // В ручном режиме уважаем выбранный адрес. В «Авто» сначала параллельно
        // измеряем все зеркала, затем пробуем восстановление от быстрого к медленному.
        // Так мёртвый основной домен не превращает валидный сохранённый токен в экран
        // входа, если доступное зеркало уже отвечает.
        let auto_mode = server_idx == 0;
        let pings = auto_mode.then(|| probe_mirrors(&mirrors));
        if let Some(pings) = &pings {
            update_mirror_labels(&app_weak, &mirrors, pings);
        }
        let mut candidates = if let Some(pings) = &pings {
            ranked_ping_indices(pings)
        } else {
            vec![server_idx.saturating_sub(1)]
        };
        if candidates.is_empty() && !mirrors.is_empty() {
            candidates.push(0);
        }

        let mut restored = None;
        let mut last_error = "Ни один сервер не ответил.".to_string();
        for index in candidates {
            let Some((name, url)) = mirrors.get(index) else {
                continue;
            };
            config.set_api_url(url);
            match restore_session(&config, token.clone()) {
                Ok(session) => {
                    restored = Some((session, name.clone(), url.clone(), index));
                    break;
                }
                Err(message) => {
                    let invalid = should_forget_saved_session(&message);
                    last_error = message;
                    if invalid {
                        break;
                    }
                }
            }
        }
        let _ = invoke_from_ui(move || {
            if let Some(app) = app_weak.upgrade() {
                app.set_session_restoring(false);
                app.set_is_loading(false);
                match restored {
                    Some((mut session, name, url, index)) => {
                        config.set_api_url(&url);
                        app.set_api_url(url);
                        if auto_mode {
                            let latency = pings
                                .as_ref()
                                .and_then(|values| values.get(index))
                                .and_then(|value| *value)
                                .map(|ms| format!(" — {ms} мс"))
                                .unwrap_or_default();
                            app.set_server_name(0, format!("{AUTO_SERVER_LABEL}: {name}"));
                            session.message =
                                format!("Сессия восстановлена через {name}{latency}.");
                        }
                        apply_session(&app, &state, &config, &generation, session);
                    }
                    None => {
                        discord_rpc::rpc_set(discord_rpc::Presence::Idle);
                        if should_forget_saved_session(&last_error) {
                            let _ = delete_token();
                            app.set_message("Сохранённая сессия истекла. Войдите снова.".into());
                        } else {
                            app.set_message(
                                format!("Не удалось восстановить сессию: {last_error}").into(),
                            );
                        }
                    }
                }
            }
        });
    });
}

fn login_and_bootstrap(
    config: &AppConfig,
    login: String,
    password: String,
    totp: String,
) -> Result<SessionData, LoginError> {
    let response = login_to_backend(config, login, password, totp)?;
    save_token(&response.token).map_err(|message| LoginError {
        message,
        requires_two_factor: false,
    })?;

    bootstrap_session(
        config,
        response.token,
        response.user,
        response.expires_at,
        response.message,
    )
    .map_err(|message| LoginError {
        message,
        requires_two_factor: false,
    })
}

fn restore_session(config: &AppConfig, token: String) -> Result<SessionData, String> {
    let user = current_user(config, &token)?;
    let expires_at = token_expiry_label(&token);
    bootstrap_session(
        config,
        token,
        user,
        expires_at,
        "Сессия восстановлена.".to_string(),
    )
}

fn token_expiry_label(token: &str) -> String {
    let Some(payload) = token.split('.').nth(1) else {
        return String::new();
    };
    let Ok(bytes) = base64::engine::general_purpose::URL_SAFE_NO_PAD.decode(payload) else {
        return String::new();
    };
    let Ok(value) = serde_json::from_slice::<Value>(&bytes) else {
        return String::new();
    };
    value
        .get("exp")
        .and_then(Value::as_u64)
        .map(format_log_timestamp)
        .unwrap_or_default()
}

fn bootstrap_session(
    config: &AppConfig,
    token: String,
    user: AuthUser,
    expires_at: String,
    message: String,
) -> Result<SessionData, String> {
    let profiles = fetch_profiles(config, &token)?;
    let selected_profile_id = choose_profile_for_user(&user.provider_uuid, &profiles)?;
    // Новости не критичны для входа: при сбое лента просто остаётся пустой.
    let news = fetch_news(config, &token);
    // Политика конфиденциальности: если пользователь не принимал текущую
    // версию — экран согласия. Сбой запроса = fail-open на клиенте
    // (сервер всё равно не выдаст launch-token без согласия).
    let policy = match fetch_policy(config) {
        Ok(p) if p.version > user.policy_accepted_version => Some(p),
        _ => None,
    };
    Ok(SessionData {
        token,
        user,
        expires_at,
        message,
        profiles,
        selected_profile_id,
        news,
        policy,
    })
}

fn apply_session(
    app: &AppWindow,
    state: &Arc<Mutex<RuntimeState>>,
    config: &AppConfig,
    generation: &Arc<AtomicU64>,
    session: SessionData,
) {
    let selected = session
        .selected_profile_id
        .as_ref()
        .and_then(|id| session.profiles.iter().find(|profile| &profile.id == id))
        .cloned();

    if let Ok(mut state) = state.lock() {
        state.token = session.token.clone();
        state.user = Some(session.user.clone());
        state.profiles = session.profiles.clone();
        state.selected_profile_id = session.selected_profile_id.clone();
    }

    app.set_requires_totp(false);
    app.set_is_authenticated(true);
    app.set_user_login(session.user.login.clone().into());
    app.set_user_uuid(session.user.provider_uuid.clone().into());
    app.set_is_slim(session.user.is_slim);
    app.set_token_expires_at(format_session_expiry(&session.expires_at).into());
    app.set_password_value(SharedString::default());
    app.set_totp_value(SharedString::default());
    app.set_message(session.message.into());

    match &session.policy {
        Some(p) => {
            app.set_policy_text(p.text.clone().into());
            app.set_policy_version_label(format!("Версия {}", p.version).into());
            app.set_policy_version(p.version);
            app.set_policy_visible(true);
        }
        None => app.set_policy_visible(false),
    }

    set_profile_ui(app, selected.as_ref());
    if let Some(profile) = selected {
        refresh_profile_install_state(
            app.as_weak(),
            Arc::clone(state),
            config.clone(),
            session.token.clone(),
            session.user.clone(),
            profile,
        );
    }

    let news_model: Vec<NewsItem> = session
        .news
        .iter()
        .map(|item| NewsItem {
            title: item.title.clone().into(),
            date: format_news_date(&item.created_at).into(),
            body: item.body.clone().into(),
        })
        .collect();
    app.set_news_items(news_model);

    apply_install_folder_label(app);

    // Запускаем фоновый SSE-слушатель: при изменении профилей на сервере
    // лаунчер перезапрашивает их без перезахода. Увеличение поколения
    // останавливает слушатель предыдущей сессии.
    let my_generation = generation.fetch_add(1, Ordering::SeqCst) + 1;
    start_profile_event_listener(
        app.as_weak(),
        Arc::clone(state),
        config.clone(),
        Arc::clone(generation),
        my_generation,
    );
    start_local_profile_watch(
        app.as_weak(),
        Arc::clone(state),
        Arc::clone(generation),
        my_generation,
    );
    discord_rpc::rpc_set(discord_rpc::Presence::Browsing {
        nick: session.user.login.clone(),
    });
}

/// Пока пользователь авторизован, быстро отслеживает удаление и обычные локальные
/// изменения управляемых файлов. Размер/mtime проверяются каждые 5 секунд; SHA-256
/// считается для файла с изменившимся mtime, а раз в минуту простоя выполняется
/// полный контроль (ловит замену с сохранёнными размером и mtime). Во время
/// установки и игры полный аудит пропускается. Серверные обновления приходят SSE.
fn start_local_profile_watch(
    app_weak: Weak<AppWindow>,
    state: Arc<Mutex<RuntimeState>>,
    generation: Arc<AtomicU64>,
    my_generation: u64,
) {
    thread::spawn(move || {
        let mut verified_mtimes = HashMap::<PathBuf, i64>::new();
        let mut audit_tick = 0_u8;
        while generation.load(Ordering::SeqCst) == my_generation {
            for _ in 0..10 {
                if generation.load(Ordering::SeqCst) != my_generation {
                    return;
                }
                thread::sleep(Duration::from_millis(500));
            }
            let snapshot = match state.lock() {
                Ok(state) => state.clone(),
                Err(_) => return,
            };
            let (Some(user), Some(profile)) = (snapshot.user.as_ref(), selected_profile(&snapshot))
            else {
                continue;
            };
            audit_tick = audit_tick.wrapping_add(1);
            let force_hash = audit_tick >= 12 && !PROFILE_SYNC_ACTIVE.load(Ordering::SeqCst);
            if audit_tick >= 12 {
                audit_tick = 0;
            }
            let detected_state =
                local_profile_files_changed(user, &profile, &mut verified_mtimes, force_hash)
                    .ok()
                    .flatten();
            let Some(detected_state) = detected_state else {
                continue;
            };
            PROFILE_CHECK_SEQUENCE.fetch_add(1, Ordering::SeqCst);
            let expected_user = user.provider_uuid.clone();
            let expected_profile = profile.id.clone();
            let app_weak = app_weak.clone();
            let current_state = Arc::clone(&state);
            let _ = invoke_from_ui(move || {
                let still_current = current_state.lock().is_ok_and(|state| {
                    state
                        .user
                        .as_ref()
                        .is_some_and(|user| user.provider_uuid == expected_user)
                        && state.selected_profile_id.as_deref() == Some(expected_profile.as_str())
                });
                if let Some(app) = app_weak.upgrade().filter(|_| still_current) {
                    set_profile_install_state(&app, detected_state);
                }
            });
        }
    });
}

fn local_profile_files_changed(
    user: &AuthUser,
    profile: &ProfileSummary,
    verified_mtimes: &mut HashMap<PathBuf, i64>,
    force_hash: bool,
) -> Result<Option<ProfileInstallState>, String> {
    let paths = profile_paths(user, &profile.id)?;
    local_profile_files_changed_at(&paths, verified_mtimes, force_hash)
}

fn local_profile_files_changed_at(
    paths: &ProfilePaths,
    verified_mtimes: &mut HashMap<PathBuf, i64>,
    force_hash: bool,
) -> Result<Option<ProfileInstallState>, String> {
    if !paths.manifest_path.is_file() {
        return Ok(Some(ProfileInstallState::Missing));
    }
    let data = fs::read_to_string(&paths.manifest_path)
        .map_err(|_| "Не удалось прочитать локальный manifest.".to_string())?;
    let local: LocalManifest =
        serde_json::from_str(&data).map_err(|_| "Локальный manifest повреждён.".to_string())?;
    for file in local.files {
        let path = safe_join(&paths.files_root, &file.path)?;
        let metadata = match fs::symlink_metadata(&path) {
            Ok(metadata) => metadata,
            Err(error) if error.kind() == std::io::ErrorKind::NotFound => {
                return Ok(Some(ProfileInstallState::UpdateAvailable));
            }
            Err(_) => return Err(format!("Не удалось проверить {}", file.path)),
        };
        if metadata.file_type().is_symlink()
            || !metadata.is_file()
            || metadata.len() != file.size.max(0) as u64
        {
            return Ok(Some(ProfileInstallState::UpdateAvailable));
        }
        let current_mtime = file_mtime_millis(&metadata);
        if force_hash
            || (current_mtime != file.mtime_millis
                && verified_mtimes.get(&path) != Some(&current_mtime))
        {
            if hash_file(&path)? != file.hash_sha256.to_lowercase() {
                return Ok(Some(ProfileInstallState::UpdateAvailable));
            }
            verified_mtimes.insert(path, current_mtime);
        }
    }
    Ok(None)
}

/// Обновляет в UI поля выбранного профиля (или показывает «Нет профилей»).
fn set_profile_ui(app: &AppWindow, selected: Option<&ProfileSummary>) {
    if let Some(profile) = selected {
        app.set_has_profile(true);
        app.set_selected_profile_name(profile.name.clone().into());
        app.set_selected_profile_version(profile.game_version.clone().into());
        set_profile_install_state(app, ProfileInstallState::Checking);
    } else {
        app.set_has_profile(false);
        app.set_profile_status("Нет профилей".into());
        app.set_profile_installed(false);
        app.set_profile_update_available(false);
        app.set_profile_state_checking(false);
        app.set_profile_state_unknown(false);
        app.set_selected_profile_name(SharedString::default());
        app.set_selected_profile_version("-".into());
    }
}

fn set_profile_install_state(app: &AppWindow, state: ProfileInstallState) {
    app.set_profile_state_checking(state == ProfileInstallState::Checking);
    app.set_profile_state_unknown(state == ProfileInstallState::Unknown);
    app.set_profile_installed(matches!(
        state,
        ProfileInstallState::Ready
            | ProfileInstallState::UpdateAvailable
            | ProfileInstallState::Unknown
    ));
    app.set_profile_update_available(state == ProfileInstallState::UpdateAvailable);
    app.set_profile_status(
        match state {
            ProfileInstallState::Checking => "Проверяем файлы",
            ProfileInstallState::Missing => "Не установлен",
            ProfileInstallState::UpdateAvailable => "Доступно обновление",
            ProfileInstallState::Ready => "Готов к запуску",
            ProfileInstallState::Unknown => "Проверка недоступна",
        }
        .into(),
    );
}

/// После входа и каждого серверного события сверяет локальную сборку с актуальным
/// manifest. Неизменённые с прошлого полного аудита файлы проверяются по размеру
/// и mtime; SHA-256 пересчитывается только для изменившихся файлов. Перед запуском
/// игры `collect_files_to_download` по-прежнему выполняет полный SHA-256-аудит.
fn refresh_profile_install_state(
    app_weak: Weak<AppWindow>,
    runtime_state: Arc<Mutex<RuntimeState>>,
    config: AppConfig,
    token: String,
    user: AuthUser,
    profile: ProfileSummary,
) {
    let check_sequence = PROFILE_CHECK_SEQUENCE.fetch_add(1, Ordering::SeqCst) + 1;
    thread::spawn(move || {
        let expected_user_uuid = user.provider_uuid.clone();
        let paths = profile_paths(&user, &profile.id);
        let had_local_manifest = paths
            .as_ref()
            .is_ok_and(|paths| paths.manifest_path.is_file());
        let install_state = match paths {
            Ok(paths) => match fetch_manifest(&config, &token, &profile.id, &paths) {
                Ok(manifest) => profile_install_state(&paths, &manifest)
                    .unwrap_or(ProfileInstallState::UpdateAvailable),
                Err(_) if had_local_manifest => ProfileInstallState::Unknown,
                Err(_) => ProfileInstallState::Missing,
            },
            Err(_) => ProfileInstallState::Missing,
        };
        let _ = invoke_from_ui(move || {
            if let Some(app) = app_weak.upgrade() {
                let still_current = runtime_state.lock().is_ok_and(|state| {
                    state
                        .user
                        .as_ref()
                        .is_some_and(|user| user.provider_uuid == expected_user_uuid)
                        && state.selected_profile_id.as_deref() == Some(profile.id.as_str())
                }) && PROFILE_CHECK_SEQUENCE.load(Ordering::SeqCst)
                    == check_sequence;
                if app.get_is_authenticated() && still_current {
                    if install_state == ProfileInstallState::Unknown
                        && app.get_profile_update_available()
                    {
                        return;
                    }
                    set_profile_install_state(&app, install_state);
                }
            }
        });
    });
}

fn profile_install_state(
    paths: &ProfilePaths,
    manifest: &Manifest,
) -> Result<ProfileInstallState, String> {
    if !paths.manifest_path.is_file() {
        return Ok(ProfileInstallState::Missing);
    }
    let data = fs::read_to_string(&paths.manifest_path)
        .map_err(|_| "Не удалось прочитать локальный manifest.".to_string())?;
    let local: LocalManifest =
        serde_json::from_str(&data).map_err(|_| "Локальный manifest повреждён.".to_string())?;
    if local.profile_id != manifest.profile.id
        || local.manifest_version != manifest.profile.manifest_version
        || local.files.len() != manifest.files.len()
    {
        return Ok(ProfileInstallState::UpdateAvailable);
    }
    let local_files = local
        .files
        .iter()
        .map(|file| (file.path.as_str(), file))
        .collect::<HashMap<_, _>>();
    if manifest.files.iter().any(|file| {
        local_files
            .get(file.path.as_str())
            .is_none_or(|local| local.hash_sha256 != file.hash_sha256 || local.size != file.size)
    }) {
        return Ok(ProfileInstallState::UpdateAvailable);
    }
    let needs_update = manifest
        .files
        .par_iter()
        .map(|file| {
            let local = local_files
                .get(file.path.as_str())
                .expect("local manifest entries checked above");
            startup_file_needs_download(&paths.files_root, file, local)
        })
        .collect::<Result<Vec<_>, String>>()?
        .into_iter()
        .any(|needs| needs);
    Ok(if needs_update {
        ProfileInstallState::UpdateAvailable
    } else {
        ProfileInstallState::Ready
    })
}

/// Фоновый поток: держит SSE-подключение к /api/profiles/events и при каждом
/// событии перезапрашивает список профилей. Завершается, когда поколение
/// сессии меняется (logout/перелогин) или токен становится недействительным.
fn start_profile_event_listener(
    app_weak: Weak<AppWindow>,
    state: Arc<Mutex<RuntimeState>>,
    config: AppConfig,
    generation: Arc<AtomicU64>,
    my_generation: u64,
) {
    thread::spawn(move || {
        let mut has_connected = false;
        while generation.load(Ordering::SeqCst) == my_generation {
            let token = match state.lock() {
                Ok(state) => state.token.clone(),
                Err(_) => return,
            };
            if token.trim().is_empty() {
                return;
            }

            match stream_profile_events(
                &config,
                &token,
                &state,
                &app_weak,
                &generation,
                my_generation,
                &mut has_connected,
            ) {
                // Сессия недействительна — повторное подключение бессмысленно.
                StreamOutcome::Unauthorized | StreamOutcome::Stopped => return,
                // Соединение закрылось/оборвалось — переподключаемся с паузой.
                StreamOutcome::Disconnected => {}
            }

            // Бэкофф перед реконнектом, прерываемый сменой поколения.
            for _ in 0..10 {
                if generation.load(Ordering::SeqCst) != my_generation {
                    return;
                }
                thread::sleep(Duration::from_millis(500));
            }
        }
    });
}

enum StreamOutcome {
    Unauthorized,
    Disconnected,
    Stopped,
}

fn stream_profile_events(
    config: &AppConfig,
    token: &str,
    state: &Arc<Mutex<RuntimeState>>,
    app_weak: &Weak<AppWindow>,
    generation: &Arc<AtomicU64>,
    my_generation: u64,
    has_connected: &mut bool,
) -> StreamOutcome {
    let client = match sse_client() {
        Ok(client) => client,
        Err(_) => return StreamOutcome::Disconnected,
    };
    let url = format!(
        "{}/api/profiles/events",
        config.api_url().trim_end_matches('/')
    );
    let response = match client
        .get(url)
        .bearer_auth(token)
        .header(reqwest::header::ACCEPT, "text/event-stream")
        .send()
    {
        Ok(response) => response,
        Err(_) => return StreamOutcome::Disconnected,
    };

    let status = response.status();
    if status == reqwest::StatusCode::UNAUTHORIZED || status == reqwest::StatusCode::FORBIDDEN {
        return StreamOutcome::Unauthorized;
    }
    if !status.is_success() {
        return StreamOutcome::Disconnected;
    }

    // Первый успешный SSE-коннект идёт сразу после bootstrap_session: список
    // профилей уже свежий, а apply_session уже запустил проверку файлов. Не
    // повторяем её. После реального разрыва делаем catch-up, поскольку SSE не
    // гарантирует replay и обновление во время офлайна могло потеряться.
    if profile_event_connection_needs_catch_up(has_connected) {
        refresh_profiles_now(config, state, app_weak);
    }

    let reader = BufReader::new(response);
    for line in reader.lines() {
        if generation.load(Ordering::SeqCst) != my_generation {
            return StreamOutcome::Stopped;
        }
        let line = match line {
            Ok(line) => line,
            // Ошибка чтения = оборванное соединение (либо истёкший heartbeat).
            Err(_) => return StreamOutcome::Disconnected,
        };
        // Строки-комментарии (heartbeat ":") и пустые строки игнорируем;
        // событие об изменении профилей несёт строка data:.
        if let Some(payload) = line.strip_prefix("data:") {
            // Брокер общий: профили шлют "profiles", релизы лаунчера —
            // "launcher-release". Незнакомый payload считаем профилями
            // (обратная совместимость).
            if payload.trim() == "launcher-release" {
                spawn_update_check(app_weak.clone(), config.clone());
            } else {
                refresh_profiles_now(config, state, app_weak);
            }
        }
    }
    StreamOutcome::Disconnected
}

fn profile_event_connection_needs_catch_up(has_connected: &mut bool) -> bool {
    std::mem::replace(has_connected, true)
}

/// Перезапрашивает профили и обновляет выбранный профиль в state и UI.
fn refresh_profiles_now(
    config: &AppConfig,
    state: &Arc<Mutex<RuntimeState>>,
    app_weak: &Weak<AppWindow>,
) {
    let token = match state.lock() {
        Ok(state) => state.token.clone(),
        Err(_) => return,
    };
    if token.trim().is_empty() {
        return;
    }

    let profiles = match fetch_profiles(config, &token) {
        Ok(profiles) => profiles,
        Err(_) => return,
    };

    let user_uuid = state
        .lock()
        .ok()
        .and_then(|state| state.user.as_ref().map(|user| user.provider_uuid.clone()));
    let selected_id =
        user_uuid.and_then(|uuid| choose_profile_for_user(&uuid, &profiles).ok().flatten());

    if let Ok(mut state) = state.lock() {
        state.profiles = profiles.clone();
        state.selected_profile_id = selected_id.clone();
    }

    let selected = selected_id
        .as_ref()
        .and_then(|id| profiles.iter().find(|profile| &profile.id == id))
        .cloned();
    let user = state.lock().ok().and_then(|state| state.user.clone());
    let app_weak = app_weak.clone();
    let install_app = app_weak.clone();
    let install_runtime = Arc::clone(state);
    let install_config = config.clone();
    let install_token = token.clone();
    let install_profile = selected.clone();
    let _ = invoke_from_ui(move || {
        if let Some(app) = app_weak.upgrade() {
            set_profile_ui(&app, selected.as_ref());
        }
    });
    if let (Some(user), Some(profile)) = (user, install_profile) {
        refresh_profile_install_state(
            install_app,
            install_runtime,
            install_config,
            install_token,
            user,
            profile,
        );
    }
}

/// HTTP-клиент для долгоживущего SSE-потока: без общего таймаута запроса,
/// но с TCP keepalive для обнаружения «мёртвых» соединений.
fn sse_client() -> Result<Client, String> {
    // SSE-поток идёт на бэкенд → защищённый канал (rustls+webpki, без прокси).
    hardened_backend_builder()
        .connect_timeout(Duration::from_secs(15))
        .tcp_keepalive(Duration::from_secs(20))
        .build()
        .map_err(|_| "Не удалось создать SSE-клиент.".to_string())
}

fn login_to_backend(
    config: &AppConfig,
    login: String,
    password: String,
    totp: String,
) -> Result<LoginResponse, LoginError> {
    let client = http_client().map_err(|_| LoginError::unavailable())?;

    let url = format!("{}/api/auth/login", config.api_url().trim_end_matches('/'));
    let response = client
        .post(url)
        .json(&LoginRequest {
            login: login.trim().to_string(),
            password,
            totp: if totp.trim().is_empty() {
                None
            } else {
                Some(totp.trim().to_string())
            },
        })
        .send()
        .map_err(|_| LoginError::unavailable())?;

    if response.status().is_success() {
        return response.json::<LoginResponse>().map_err(|_| LoginError {
            message: "Backend вернул некорректный ответ.".to_string(),
            requires_two_factor: false,
        });
    }

    let status = response.status();
    let error = response.json::<ErrorResponse>().unwrap_or(ErrorResponse {
        message: None,
        requires_two_factor: None,
    });

    Err(LoginError {
        message: error
            .message
            .unwrap_or_else(|| format!("Ошибка авторизации: HTTP {}", status.as_u16())),
        requires_two_factor: error.requires_two_factor.unwrap_or(false),
    })
}

fn current_user(config: &AppConfig, token: &str) -> Result<AuthUser, String> {
    let client = http_client()?;
    let response = client
        .get(format!(
            "{}/api/auth/me",
            config.api_url().trim_end_matches('/')
        ))
        .bearer_auth(token)
        .send()
        .map_err(|_| "Backend лаунчера недоступен.".to_string())?;
    parse_json_response(response, "Не удалось восстановить пользователя")
}

fn fetch_profiles(config: &AppConfig, token: &str) -> Result<Vec<ProfileSummary>, String> {
    let client = http_client()?;
    let response = client
        .get(format!(
            "{}/api/profiles",
            config.api_url().trim_end_matches('/')
        ))
        .bearer_auth(token)
        .send()
        .map_err(|_| "Не удалось получить профили проекта.".to_string())?;
    parse_json_response(response, "Backend вернул некорректный список профилей")
}

// Текст и версия Политики конфиденциальности (публичный эндпоинт, без auth).
fn fetch_policy(config: &AppConfig) -> Result<PolicyInfo, String> {
    let client = http_client()?;
    let response = client
        .get(format!(
            "{}/api/policy",
            config.api_url().trim_end_matches('/')
        ))
        .send()
        .map_err(|_| "Backend лаунчера недоступен.".to_string())?;
    parse_json_response(response, "Backend вернул некорректную политику")
}

// Фиксирует согласие на сервере. 409 = версия успела смениться.
fn accept_policy(config: &AppConfig, token: &str, version: i32) -> Result<(), String> {
    let client = http_client()?;
    let response = client
        .post(format!(
            "{}/api/policy/accept",
            config.api_url().trim_end_matches('/')
        ))
        .bearer_auth(token)
        .json(&serde_json::json!({ "version": version }))
        .send()
        .map_err(|_| "Backend лаунчера недоступен.".to_string())?;
    if response.status().is_success() {
        return Ok(());
    }
    Err("Не удалось сохранить согласие. Попробуйте ещё раз.".to_string())
}

fn fetch_news(config: &AppConfig, token: &str) -> Vec<NewsSummary> {
    let client = match http_client() {
        Ok(client) => client,
        Err(_) => return Vec::new(),
    };
    let url = format!(
        "{}/api/news?limit=20",
        config.api_url().trim_end_matches('/')
    );
    let response = match client.get(url).bearer_auth(token).send() {
        Ok(response) => response,
        Err(_) => return Vec::new(),
    };
    if !response.status().is_success() {
        return Vec::new();
    }
    response.json::<Vec<NewsSummary>>().unwrap_or_default()
}

// Превращает ISO-дату Telegram (2026-06-08T12:30:00+00:00) в формат ДД.ММ.ГГГГ.
fn format_news_date(raw: &str) -> String {
    let date_part = raw.split('T').next().unwrap_or(raw);
    let pieces: Vec<&str> = date_part.split('-').collect();
    if pieces.len() == 3 {
        format!("{}.{}.{}", pieces[2], pieces[1], pieces[0])
    } else {
        date_part.to_string()
    }
}

#[derive(Debug, Deserialize)]
#[serde(rename_all = "camelCase")]
struct YggdrasilSession {
    access_token: String,
    uuid: String,
    name: String,
}

// Перед запуском обмениваем JWT лаунчера на игровую сессию (Minecraft accessToken),
// которую распознаёт наш Yggdrasil-сервер. Без неё игрок не пройдёт join на сервере.
fn fetch_yggdrasil_session(
    config: &AppConfig,
    token: &str,
    nonce: &str,
) -> Result<YggdrasilSession, String> {
    let client = http_client()?;
    let url = format!(
        "{}/api/yggdrasil/launcher-session",
        config.api_url().trim_end_matches('/')
    );
    // nonce связывает игровую сессию с launch-token античита (confirm пометит её Verified).
    let response = client
        .post(url)
        .bearer_auth(token)
        .json(&serde_json::json!({ "nonce": nonce }))
        .send()
        .map_err(|_| "Не удалось получить игровую сессию.".to_string())?;
    if !response.status().is_success() {
        return Err(format!(
            "Сервер аутентификации отклонил сессию: HTTP {}",
            response.status().as_u16()
        ));
    }
    response
        .json::<YggdrasilSession>()
        .map_err(|_| "Некорректный ответ игровой сессии.".to_string())
}

// Интервал keepalive-пинга игровой сессии: с большим запасом под 15-мин TTL сессии,
// переживает несколько пропущенных пингов (сетевые сбои).
const KEEPALIVE_INTERVAL: Duration = Duration::from_secs(120);

// Пока процесс игры жив (stop не взведён), периодически продлевает игровую сессию по
// nonce. По nonce, а не accessToken, чтобы продление пережило /authserver/refresh
// (authlib-injector может сменить токен). Спим короткими квантами для отзывчивого
// завершения на закрытии игры. Сетевой сбой не фатален: сессия живёт по TTL, следующий
// пинг наверстает.
fn session_keepalive_loop(api_url: &str, token: &str, nonce: &str, stop: &AtomicBool) {
    let client = match http_client() {
        Ok(c) => c,
        Err(_) => return,
    };
    let url = format!(
        "{}/api/yggdrasil/launcher-session/keepalive",
        api_url.trim_end_matches('/')
    );
    anticheat::poll_until(stop, KEEPALIVE_INTERVAL, || {
        let _ = client
            .post(&url)
            .bearer_auth(token)
            .json(&serde_json::json!({ "nonce": nonce }))
            .send();
    });
}

// Гарантирует наличие authlib-injector.jar в служебной папке лаунчера (вне
// files/, чтобы cleanup его не удалял). Качает с бэкенда при отсутствии.
fn ensure_authlib_injector(
    config: &AppConfig,
    expected_sha: Option<&str>,
) -> Result<Option<PathBuf>, String> {
    let Some(dir) = project_dirs().ok().map(|d| d.data_dir().to_path_buf()) else {
        return Ok(None);
    };
    let path = dir.join("authlib-injector.jar");
    let url = format!(
        "{}/api/yggdrasil/authlib-injector.jar",
        config.api_url().trim_end_matches('/')
    );
    let client = backend_download_client()?;
    artifacts::ensure(&client, &url, &path, &dir, expected_sha).map_err(|e| e.message())
}

// После закрытия игры гасим accessToken, чтобы скопированную команду запуска
// нельзя было переиспользовать позже. Best-effort: ошибки игнорируем.
fn invalidate_yggdrasil_session(config: &AppConfig, access_token: &str) {
    let Ok(client) = http_client() else {
        return;
    };
    let url = format!(
        "{}/api/yggdrasil/authserver/invalidate",
        config.api_url().trim_end_matches('/')
    );
    let _ = client
        .post(url)
        .json(&serde_json::json!({ "accessToken": access_token }))
        .send();
}

fn fetch_manifest(
    config: &AppConfig,
    token: &str,
    profile_id: &str,
    paths: &ProfilePaths,
) -> Result<Manifest, String> {
    let client = http_client()?;
    let url = format!(
        "{}/api/profiles/{}/manifest",
        config.api_url().trim_end_matches('/'),
        profile_id
    );
    let cache_path = paths.profile_root.join("manifest.remote.json");
    let etag_path = paths.profile_root.join("manifest.remote.etag");
    let cached_etag = fs::read_to_string(&etag_path).ok();

    let mut request = client.get(&url).bearer_auth(token);
    if let Some(etag) = cached_etag
        .as_deref()
        .map(str::trim)
        .filter(|etag| !etag.is_empty())
    {
        request = request.header(reqwest::header::IF_NONE_MATCH, etag);
    }
    let mut response = request
        .send()
        .map_err(|_| "Не удалось получить manifest профиля.".to_string())?;

    if response.status() == reqwest::StatusCode::NOT_MODIFIED {
        if let Ok(data) = fs::read(&cache_path) {
            if let Ok(manifest) = serde_json::from_slice::<Manifest>(&data) {
                return Ok(manifest);
            }
        }
        // 304 без пригодного локального тела возможен после ручной чистки или
        // аварийного выключения между записью ETag и JSON. Один раз повторяем
        // запрос без условия и восстанавливаем кэш.
        let _ = fs::remove_file(&etag_path);
        response = client
            .get(&url)
            .bearer_auth(token)
            .send()
            .map_err(|_| "Не удалось повторно получить manifest профиля.".to_string())?;
    }

    if !response.status().is_success() {
        return parse_json_response(response, "Backend вернул некорректный manifest");
    }

    let response_etag = response
        .headers()
        .get(reqwest::header::ETAG)
        .and_then(|value| value.to_str().ok())
        .map(str::to_owned);
    let data = response
        .bytes()
        .map_err(|error| format!("Не удалось прочитать manifest профиля: {error}"))?;
    let manifest = serde_json::from_slice::<Manifest>(&data)
        .map_err(|error| format!("Backend вернул некорректный manifest: {error}"))?;

    // Кэш — ускорение, не условие запуска: ошибка записи не мешает использовать
    // уже проверенный ответ текущего запроса.
    if write_manifest_cache(&cache_path, &data).is_ok() {
        if let Some(etag) = response_etag {
            let _ = write_manifest_cache(&etag_path, etag.as_bytes());
        }
    }
    Ok(manifest)
}

fn write_manifest_cache(path: &Path, data: &[u8]) -> std::io::Result<()> {
    if let Some(parent) = path.parent() {
        fs::create_dir_all(parent)?;
    }
    let sequence = MANIFEST_CACHE_SEQUENCE.fetch_add(1, Ordering::Relaxed);
    let mut temp_name = path.as_os_str().to_os_string();
    temp_name.push(format!(".{}.{}.part", std::process::id(), sequence));
    let temp = PathBuf::from(temp_name);
    fs::write(&temp, data)?;
    let result = (|| {
        if path.exists() {
            fs::remove_file(path)?;
        }
        fs::rename(&temp, path)
    })();
    if result.is_err() {
        let _ = fs::remove_file(temp);
    }
    result
}

fn parse_json_response<T: for<'de> Deserialize<'de>>(
    response: reqwest::blocking::Response,
    fallback: &str,
) -> Result<T, String> {
    let status = response.status();
    if status.is_success() {
        // Причину не глотаем: reqwest сюда же отдаёт обрыв связи и таймаут посреди
        // чтения тела (у http_client 30с на весь запрос, а manifest — самый крупный
        // ответ API). Без неё «некорректный manifest» врёт про причину и уводит
        // диагностику в сторону разбора JSON.
        return response
            .json::<T>()
            .map_err(|err| format!("{fallback}: {err}"));
    }

    let error = response.json::<ErrorResponse>().unwrap_or(ErrorResponse {
        message: None,
        requires_two_factor: None,
    });
    Err(error
        .message
        .unwrap_or_else(|| format!("HTTP {}", status.as_u16())))
}

fn sync_and_launch(
    config: &AppConfig,
    token: &str,
    user: &AuthUser,
    profile: &ProfileSummary,
    app: &Weak<AppWindow>,
) -> Result<String, String> {
    post_progress(app, "Получаем профиль", &profile.name, "0%", 0.0, true);
    let paths = profile_paths(user, &profile.id)?;
    let manifest = fetch_manifest(config, token, &profile.id, &paths)?;
    if manifest.profile.id != profile.id {
        return Err("Backend вернул manifest другого профиля.".to_string());
    }
    ensure_directory(&paths.profile_root, "Не удалось создать папку профиля.")?;
    ensure_directory(&paths.files_root, "Не удалось создать папку профиля.")?;

    post_progress(
        app,
        "Проверяем файлы",
        &format!("{} файлов", manifest.file_count),
        "0%",
        0.04,
        true,
    );
    let files_to_download = collect_files_to_download(app, &paths.files_root, &manifest.files)?;
    let missing_bytes = files_to_download
        .iter()
        .map(|file| file.size.max(0) as u64)
        .sum();
    if bundle::should_use(
        manifest.bundle.as_ref(),
        files_to_download.len(),
        missing_bytes,
    ) {
        let bundle = manifest.bundle.as_ref().expect("checked by should_use");
        let url = absolute_api_url(config, &bundle.download_url);
        let specs = manifest
            .files
            .iter()
            .map(|file| bundle::FileSpec {
                path: file.path.clone(),
                hash_sha256: file.hash_sha256.clone(),
                size: file.size,
                executable: file.executable,
            })
            .collect::<Vec<_>>();
        let client = backend_download_client()?;
        bundle::download_and_install(
            &client,
            &url,
            token,
            &paths.profile_root,
            &paths.files_root,
            bundle,
            &specs,
            |downloaded, total| {
                let fraction = if total == 0 {
                    0.0
                } else {
                    downloaded as f32 / total as f32
                };
                post_progress(
                    app,
                    "Скачиваем сборку",
                    &format!("bundle v{}", bundle.build_id),
                    &format!(
                        "{} / {}",
                        format_bytes(downloaded as i64),
                        format_bytes(total as i64)
                    ),
                    0.22 + fraction * 0.70,
                    true,
                );
            },
        )?;
    } else {
        download_files(
            config,
            token,
            app,
            &paths.files_root,
            &manifest,
            &files_to_download,
        )?;
    }

    post_progress(
        app,
        "Проверяем Java",
        "Runtime текущей ОС",
        "92%",
        0.92,
        true,
    );
    let java_managed_paths = ensure_java_runtime(app, &paths, &manifest)?;

    post_progress(
        app,
        "Очищаем",
        "Удаляем устаревшие файлы",
        "96%",
        0.96,
        true,
    );
    cleanup_unmanaged_files(&paths, &manifest, &java_managed_paths)?;
    save_local_manifest(&paths.manifest_path, &paths.files_root, &manifest)?;

    post_progress(app, "Запускаем", &manifest.profile.name, "99%", 0.99, true);
    launch_profile(app, config, &paths, &manifest, token, &user.login)
}

fn collect_files_to_download(
    app: &Weak<AppWindow>,
    files_root: &Path,
    files: &[ManifestFile],
) -> Result<Vec<ManifestFile>, String> {
    let total = files.len().max(1);
    let processed = AtomicUsize::new(0);

    // Проверяем файлы параллельно по всем ядрам. Каждый файл по-прежнему
    // полностью сверяется по SHA256 с backend manifest — модель безопасности
    // не меняется, ускоряется только пропускная способность хеширования.
    // `collect` в rayon сохраняет исходный порядок манифеста.
    let checked = files
        .par_iter()
        .map(|file| -> Result<Option<ManifestFile>, String> {
            let needs = needs_download(files_root, file)?;
            let done = processed.fetch_add(1, Ordering::Relaxed) + 1;
            post_progress(
                app,
                "Проверяем файлы",
                &file.path,
                &format!("{}/{}", done, files.len()),
                0.04 + (done as f32 / total as f32) * 0.16,
                true,
            );
            Ok(needs.then(|| file.clone()))
        })
        .collect::<Result<Vec<_>, String>>()?;

    Ok(checked.into_iter().flatten().collect())
}

fn download_files(
    config: &AppConfig,
    token: &str,
    app: &Weak<AppWindow>,
    files_root: &Path,
    manifest: &Manifest,
    files: &[ManifestFile],
) -> Result<(), String> {
    if files.is_empty() {
        post_progress(
            app,
            "Скачиваем",
            "Все файлы уже актуальны",
            "92%",
            0.92,
            true,
        );
        return Ok(());
    }

    // Два клиента: защищённый для файлов со своего бэкенда (rustls+webpki+JWT) и обычный
    // для файлов с публичного S3-зеркала (download_one_file выбирает по is_api_url).
    let backend_client = backend_download_client()?;
    let asset_client = download_client()?;
    let total_bytes = files
        .iter()
        .map(|file| file.size.max(0) as u64)
        .sum::<u64>()
        .max(1);
    let completed_bytes = AtomicU64::new(0);
    let completed_files = AtomicUsize::new(0);
    let total_files = files.len();

    // Файлы качаются параллельно пулом воркеров. На профиле из множества мелких
    // файлов узкое место — задержка (RTT) на каждый запрос, а не канал, поэтому
    // перекрытие запросов даёт кратное ускорение. Воркеров берём больше числа ядер,
    // т.к. работа I/O-bound (потоки большую часть времени ждут сеть). Модель
    // безопасности не меняется: каждый файл по-прежнему сверяется по SHA256 и
    // атомарно переименовывается из временного файла.
    let workers = total_files.clamp(1, 16);
    let pool = rayon::ThreadPoolBuilder::new()
        .num_threads(workers)
        .build()
        .map_err(|_| "Не удалось создать пул загрузки.".to_string())?;

    pool.install(|| {
        files
            .par_iter()
            .map(|file| -> Result<(), String> {
                let file_bytes = download_one_file(
                    &backend_client,
                    &asset_client,
                    config,
                    token,
                    files_root,
                    file,
                )?;

                let done_files = completed_files.fetch_add(1, Ordering::Relaxed) + 1;
                let done_bytes =
                    completed_bytes.fetch_add(file_bytes, Ordering::Relaxed) + file_bytes;
                let progress = 0.22 + (done_bytes as f32 / total_bytes as f32) * 0.70;
                post_progress(
                    app,
                    "Скачиваем",
                    &file.path,
                    &format!("{}/{}", done_files, total_files),
                    progress.min(0.92),
                    true,
                );
                Ok(())
            })
            .collect::<Result<Vec<_>, String>>()
    })?;

    post_progress(
        app,
        "Скачиваем",
        &format!(
            "{} файлов, {}",
            manifest.file_count,
            format_bytes(manifest.total_size)
        ),
        "92%",
        0.92,
        true,
    );
    Ok(())
}

// Скачивает один файл во временный путь, сверяет SHA256 и размер, затем атомарно
// переименовывает в целевой путь. Вызывается параллельно из пула в download_files;
// все пути уникальны на файл, поэтому конкурентная запись безопасна.
fn download_one_file(
    backend_client: &Client,
    asset_client: &Client,
    config: &AppConfig,
    token: &str,
    files_root: &Path,
    file: &ManifestFile,
) -> Result<u64, String> {
    let target = safe_join(files_root, &file.path)?;
    if let Some(parent) = target.parent() {
        fs::create_dir_all(parent)
            .map_err(|_| "Не удалось создать папку для файла.".to_string())?;
    }

    let url = absolute_api_url(config, &file.download_url);
    // Свой бэкенд — защищённый клиент (rustls+webpki, без прокси) + JWT. Публичное
    // зеркало (S3-бакет) — обычный клиент без токена: JWT туда слать и незачем, и
    // вредно (S3 отвечает 400 на Authorization), а его CA может быть вне webpki.
    let is_backend = is_api_url(config, &url);
    let client = if is_backend {
        backend_client
    } else {
        asset_client
    };
    let mut request = client.get(&url);
    if is_backend {
        request = request.bearer_auth(token);
    }
    let mut response = request
        .send()
        .map_err(|_| format!("Не удалось скачать {}", file.path))?;
    if !response.status().is_success() {
        return Err(format!(
            "Ошибка скачивания {}: HTTP {}",
            file.path,
            response.status().as_u16()
        ));
    }

    let temp_path = temp_download_path(&target);
    let mut output =
        File::create(&temp_path).map_err(|_| format!("Не удалось записать {}", file.path))?;
    let mut hasher = Sha256::new();
    let mut file_bytes = 0_u64;
    let mut buffer = [0_u8; 64 * 1024];

    loop {
        let read = response
            .read(&mut buffer)
            .map_err(|_| format!("Ошибка чтения {}", file.path))?;
        if read == 0 {
            break;
        }
        output
            .write_all(&buffer[..read])
            .map_err(|_| format!("Ошибка записи {}", file.path))?;
        hasher.update(&buffer[..read]);
        file_bytes += read as u64;
    }
    output
        .flush()
        .map_err(|_| format!("Ошибка записи {}", file.path))?;

    let hash = hex_hash(hasher.finalize().as_slice());
    if hash != file.hash_sha256.to_lowercase() {
        let _ = fs::remove_file(&temp_path);
        return Err(format!("Hash mismatch: {}", file.path));
    }
    if file.size >= 0 && file_bytes != file.size as u64 {
        let _ = fs::remove_file(&temp_path);
        return Err(format!("Размер файла изменился: {}", file.path));
    }

    set_manifest_executable(&temp_path, file.executable)?;

    remove_existing_path_for_replace(&target)
        .map_err(|_| format!("Не удалось заменить {}", file.path))?;
    fs::rename(&temp_path, &target).map_err(|_| format!("Не удалось сохранить {}", file.path))?;
    Ok(file_bytes)
}

fn ensure_java_runtime(
    app: &Weak<AppWindow>,
    paths: &ProfilePaths,
    manifest: &Manifest,
) -> Result<HashSet<String>, String> {
    let java_rel = os_value(
        &manifest.profile.java_path_windows,
        &manifest.profile.java_path_linux,
        &manifest.profile.java_path_macos,
    )
    .trim();
    if java_rel.is_empty() {
        return Err("В профиле не указан Java runtime для этой ОС.".to_string());
    }

    let platform_key = java_runtime_platform_key();
    let component = java_runtime_component(manifest.profile.java_version);
    let executable_rel = java_runtime_executable_rel(platform_key);
    let java_root_rel = java_runtime_root_rel(java_rel, executable_rel)?;
    let java_root = safe_join(&paths.files_root, &java_root_rel)?;
    ensure_directory(&java_root, "Не удалось создать папку Java runtime.")?;

    let client = download_client()?;
    let index_response = client
        .get(JAVA_RUNTIME_INDEX_URL)
        .send()
        .map_err(|_| "Не удалось получить список Java runtime.".to_string())?;
    let index: JavaRuntimeIndex =
        parse_json_response(index_response, "Не удалось прочитать список Java runtime.")?;
    let release = index
        .get(platform_key)
        .and_then(|platform| platform.get(component))
        .and_then(|releases| releases.first())
        .ok_or_else(|| {
            format!(
                "Java runtime {} для платформы {} не найден.",
                component, platform_key
            )
        })?;

    post_progress(
        app,
        "Проверяем Java",
        &format!("{} {}", platform_key, component),
        "manifest",
        0.925,
        true,
    );
    let manifest_bytes = fetch_sha1_bytes(
        &client,
        &release.manifest.url,
        &release.manifest.sha1,
        release.manifest.size,
        "Java runtime manifest",
    )?;
    let runtime_manifest: JavaRuntimeManifest = serde_json::from_slice(&manifest_bytes)
        .map_err(|_| "Java runtime manifest повреждён.".to_string())?;
    let managed_paths = java_runtime_managed_paths(&java_root_rel, &runtime_manifest);

    prepare_java_directories(&java_root, &runtime_manifest)?;
    let tasks = collect_java_download_tasks(app, &java_root, &runtime_manifest)?;
    download_java_files(app, &client, &java_root, &tasks)?;
    prepare_java_links(&java_root, &runtime_manifest)?;

    let java_path = safe_join(&paths.files_root, java_rel)?;
    if !java_path.exists() {
        return Err(format!(
            "Java runtime скачан, но путь профиля неверный для этой ОС. Укажи {}{}{}",
            java_root_rel,
            if java_root_rel.is_empty() { "" } else { "/" },
            executable_rel
        ));
    }
    ensure_executable(&java_path, true)?;
    Ok(managed_paths)
}

fn prepare_java_directories(root: &Path, manifest: &JavaRuntimeManifest) -> Result<(), String> {
    for (path, entry) in &manifest.files {
        if entry.kind != "directory" {
            continue;
        }
        let target = safe_join(root, path)?;
        ensure_directory(
            &target,
            &format!("Не удалось создать папку Java runtime: {}", path),
        )?;
    }
    Ok(())
}

fn collect_java_download_tasks(
    app: &Weak<AppWindow>,
    root: &Path,
    manifest: &JavaRuntimeManifest,
) -> Result<Vec<JavaRuntimeDownloadTask>, String> {
    let mut files = manifest
        .files
        .iter()
        .filter_map(|(path, entry)| {
            let download = entry.downloads.as_ref()?.raw.as_ref()?;
            Some((path, entry, download))
        })
        .collect::<Vec<_>>();
    files.sort_by(|(left, _, _), (right, _, _)| left.cmp(right));

    let total = files.len().max(1);
    let processed = AtomicUsize::new(0);

    // Параллельная проверка Java runtime: SHA1 каждого файла по-прежнему
    // полностью сверяется с manifest, меняется только скорость хеширования.
    let tasks = files
        .par_iter()
        .map(
            |&(path, entry, download)| -> Result<Option<JavaRuntimeDownloadTask>, String> {
                let needs = java_file_needs_download(root, path, download)?;
                let done = processed.fetch_add(1, Ordering::Relaxed) + 1;
                post_progress(
                    app,
                    "Проверяем Java",
                    path,
                    &format!("{}/{}", done, total),
                    0.925 + (done as f32 / total as f32) * 0.015,
                    true,
                );
                if needs {
                    Ok(Some(JavaRuntimeDownloadTask {
                        path: path.clone(),
                        download: download.clone(),
                        executable: entry.executable,
                    }))
                } else {
                    let target = safe_join(root, path)?;
                    ensure_executable(&target, entry.executable)?;
                    Ok(None)
                }
            },
        )
        .collect::<Result<Vec<_>, String>>()?;

    Ok(tasks.into_iter().flatten().collect())
}

fn download_java_files(
    app: &Weak<AppWindow>,
    client: &Client,
    root: &Path,
    tasks: &[JavaRuntimeDownloadTask],
) -> Result<(), String> {
    if tasks.is_empty() {
        post_progress(
            app,
            "Проверяем Java",
            "Java runtime актуален",
            "94%",
            0.94,
            true,
        );
        return Ok(());
    }

    let total_bytes = tasks
        .iter()
        .map(|task| task.download.size.max(0) as u64)
        .sum::<u64>()
        .max(1);
    let mut completed_bytes = 0_u64;

    for (index, task) in tasks.iter().enumerate() {
        let target = safe_join(root, &task.path)?;
        if let Some(parent) = target.parent() {
            fs::create_dir_all(parent)
                .map_err(|_| format!("Не удалось создать папку Java: {}", task.path))?;
        }

        let mut response = client
            .get(&task.download.url)
            .send()
            .map_err(|_| format!("Не удалось скачать Java файл {}", task.path))?;
        if !response.status().is_success() {
            return Err(format!(
                "Ошибка скачивания Java {}: HTTP {}",
                task.path,
                response.status().as_u16()
            ));
        }

        let temp_path = temp_download_path(&target);
        let mut output =
            File::create(&temp_path).map_err(|_| format!("Не удалось записать {}", task.path))?;
        let mut hasher = Sha1::new();
        let mut file_bytes = 0_u64;
        let mut buffer = [0_u8; 64 * 1024];

        loop {
            let read = response
                .read(&mut buffer)
                .map_err(|_| format!("Ошибка чтения Java {}", task.path))?;
            if read == 0 {
                break;
            }
            output
                .write_all(&buffer[..read])
                .map_err(|_| format!("Ошибка записи Java {}", task.path))?;
            hasher.update(&buffer[..read]);
            file_bytes += read as u64;

            let progress_bytes = completed_bytes + file_bytes;
            let progress = 0.94 + (progress_bytes as f32 / total_bytes as f32) * 0.04;
            post_progress(
                app,
                "Скачиваем Java",
                &task.path,
                &format!("{}/{}", index + 1, tasks.len()),
                progress.min(0.98),
                true,
            );
        }
        output
            .flush()
            .map_err(|_| format!("Ошибка записи Java {}", task.path))?;

        let hash = hex_hash(hasher.finalize().as_slice());
        if hash != task.download.sha1.to_lowercase() {
            let _ = fs::remove_file(&temp_path);
            return Err(format!("Hash mismatch Java: {}", task.path));
        }
        if task.download.size >= 0 && file_bytes != task.download.size as u64 {
            let _ = fs::remove_file(&temp_path);
            return Err(format!("Размер Java файла изменился: {}", task.path));
        }

        remove_existing_path_for_replace(&target)
            .map_err(|_| format!("Не удалось заменить {}", task.path))?;
        fs::rename(&temp_path, &target)
            .map_err(|_| format!("Не удалось сохранить Java {}", task.path))?;
        ensure_executable(&target, task.executable)?;
        completed_bytes += file_bytes;
    }
    Ok(())
}

fn prepare_java_links(root: &Path, manifest: &JavaRuntimeManifest) -> Result<(), String> {
    for (path, entry) in &manifest.files {
        if entry.kind != "link" {
            continue;
        }
        create_java_link(root, path, &entry.target)?;
    }
    Ok(())
}

fn java_file_needs_download(
    root: &Path,
    rel: &str,
    download: &JavaRuntimeDownload,
) -> Result<bool, String> {
    let target = safe_join(root, rel)?;
    let metadata = match fs::symlink_metadata(&target) {
        Ok(metadata) => metadata,
        Err(error) if error.kind() == std::io::ErrorKind::NotFound => return Ok(true),
        Err(_) => return Err(format!("Не удалось проверить Java файл {}", rel)),
    };
    if metadata.file_type().is_symlink()
        || !metadata.is_file()
        || metadata.len() != download.size as u64
    {
        return Ok(true);
    }
    let hash = hash_file_sha1(&target)?;
    Ok(hash != download.sha1.to_lowercase())
}

fn needs_download(files_root: &Path, file: &ManifestFile) -> Result<bool, String> {
    let target = safe_join(files_root, &file.path)?;
    let metadata = match fs::symlink_metadata(&target) {
        Ok(metadata) => metadata,
        Err(error) if error.kind() == std::io::ErrorKind::NotFound => return Ok(true),
        Err(_) => return Err(format!("Не удалось проверить {}", file.path)),
    };
    if metadata.file_type().is_symlink()
        || !metadata.is_file()
        || metadata.len() != file.size as u64
    {
        return Ok(true);
    }

    let expected = file.hash_sha256.to_lowercase();
    let hash = hash_file(&target)?;
    if hash != expected {
        return Ok(true);
    }
    set_manifest_executable(&target, file.executable)?;
    Ok(false)
}

/// Быстрая проверка для статуса профиля при старте. Локальный manifest записан
/// только после полного SHA-аудита/установки, поэтому совпавшие размер и mtime
/// означают, что файл с прошлого аудита не менялся. Если mtime отличается (или
/// отсутствует у старого manifest), переходим к строгой SHA-256-проверке.
fn startup_file_needs_download(
    files_root: &Path,
    file: &ManifestFile,
    local: &LocalFileRecord,
) -> Result<bool, String> {
    let target = safe_join(files_root, &file.path)?;
    let metadata = match fs::symlink_metadata(&target) {
        Ok(metadata) => metadata,
        Err(error) if error.kind() == std::io::ErrorKind::NotFound => return Ok(true),
        Err(_) => return Err(format!("Не удалось проверить {}", file.path)),
    };
    if metadata.file_type().is_symlink()
        || !metadata.is_file()
        || metadata.len() != file.size.max(0) as u64
    {
        return Ok(true);
    }

    let current_mtime = file_mtime_millis(&metadata);
    if local.mtime_millis > 0 && current_mtime == local.mtime_millis {
        return Ok(false);
    }

    needs_download(files_root, file)
}

fn file_mtime_millis(metadata: &fs::Metadata) -> i64 {
    metadata
        .modified()
        .ok()
        .and_then(|time| time.duration_since(UNIX_EPOCH).ok())
        .map(|elapsed| elapsed.as_millis() as i64)
        .unwrap_or(0)
}

fn cleanup_unmanaged_files(
    paths: &ProfilePaths,
    manifest: &Manifest,
    java_managed_paths: &HashSet<String>,
) -> Result<(), String> {
    let mut allowed_paths = manifest
        .files
        .iter()
        .filter_map(|file| normalize_relative_path(&file.path))
        .collect::<HashSet<_>>();
    allowed_paths.extend(java_managed_paths.iter().cloned());

    let preserve_paths = normalize_preserve_paths(&manifest.preserve_paths);
    cleanup_directory(
        &paths.files_root,
        &paths.files_root,
        &allowed_paths,
        &preserve_paths,
    )
}

fn cleanup_directory(
    root: &Path,
    current: &Path,
    allowed_paths: &HashSet<String>,
    preserve_paths: &[String],
) -> Result<(), String> {
    if !current.exists() {
        return Ok(());
    }

    let entries = fs::read_dir(current)
        .map_err(|_| format!("Не удалось прочитать папку {}", current.to_string_lossy()))?;
    for entry in entries {
        let entry = entry.map_err(|_| "Не удалось прочитать файл профиля.".to_string())?;
        let path = entry.path();
        let rel = relative_path(root, &path)?;
        if preserve_path_matches(&rel, preserve_paths) {
            continue;
        }

        let metadata =
            fs::symlink_metadata(&path).map_err(|_| format!("Не удалось проверить {}", rel))?;
        if metadata.is_dir() && !metadata.file_type().is_symlink() {
            cleanup_directory(root, &path, allowed_paths, preserve_paths)?;
            let _ = fs::remove_dir(&path);
            continue;
        }

        if allowed_paths.contains(&rel) {
            continue;
        }

        remove_existing_path_for_replace(&path)
            .map_err(|_| format!("Не удалось удалить лишний файл {}", rel))?;
    }
    Ok(())
}

fn launch_profile(
    app: &Weak<AppWindow>,
    config: &AppConfig,
    paths: &ProfilePaths,
    manifest: &Manifest,
    token: &str,
    nick: &str,
) -> Result<String, String> {
    // Pre-launch античит: скан процессов, HWID, handshake (баны/форс-апдейт), манифест
    // целостности. Блокирует запуск (Err) при бане/форс-апдейте. nonce связывает сессию.
    let mut guard = anticheat::LaunchGuard::begin(config, token)?;

    // Игровая сессия привязывается к nonce из handshake/init. confirm выполняет
    // Java-агент внутри JVM (M3) — без него сессия не Verified и сервер отклонит join.
    let session = fetch_yggdrasil_session(config, token, guard.nonce())?;

    let java_rel = os_value(
        &manifest.profile.java_path_windows,
        &manifest.profile.java_path_linux,
        &manifest.profile.java_path_macos,
    );
    let command_template = os_value(
        &manifest.profile.launch_command_windows,
        &manifest.profile.launch_command_linux,
        &manifest.profile.launch_command_macos,
    );
    if java_rel.trim().is_empty() {
        return Err("В профиле не указан Java runtime для этой ОС.".to_string());
    }
    if command_template.trim().is_empty() {
        return Err("В профиле не указана команда запуска для этой ОС.".to_string());
    }

    let java_path = safe_join(&paths.files_root, java_rel)?;
    if !java_path.exists() {
        return Err(format!("Java runtime не найден: {}", java_rel));
    }

    let settings = load_settings().unwrap_or_default();
    let mut jvm_args =
        jvm_args_with_memory(&manifest.profile.jvm_args, effective_memory_gb(&settings))?;

    // Подключаем authlib-injector как javaagent, указывая на наш Yggdrasil-сервер.
    // Jar — launcher-managed (качается с бэкенда), лежит вне files/, поэтому
    // cleanup его не трогает. Клиент и игровой сервер должны указывать на один
    // и тот же базовый URL (GML-совместимый путь). SHA — из манифеста целостности guard.
    if let Some(injector) = ensure_authlib_injector(config, guard.authlib_sha())? {
        let ygg_url = format!(
            "{}/api/v1/integrations/authlib/minecraft",
            config.api_url().trim_end_matches('/')
        );
        jvm_args.insert(
            0,
            format!("-javaagent:{}={}", injector.to_string_lossy(), ygg_url),
        );
    }

    // Инжект агентов античита в начало jvm_args (native + Java-агент с SHA-сверкой).
    // No-op, если handshake не выдал токен (fail-open) — тогда сессия не пройдёт
    // verified-гейт на join. Err — подмена артефакта (блок запуска).
    guard.inject_into(&mut jvm_args, config)?;

    let values = PlaceholderValues {
        java: java_path.to_string_lossy().to_string(),
        game_dir: paths.files_root.to_string_lossy().to_string(),
        profile_dir: paths.profile_root.to_string_lossy().to_string(),
        login: session.name.clone(),
        uuid: session.uuid.clone(),
        access_token: session.access_token.clone(),
        jvm_args,
    };
    let mut command = render_command(command_template, &values)?;
    remove_module_path_entries_from_classpath(&mut command);
    if command.is_empty() {
        return Err("Команда запуска пуста.".to_string());
    }

    // Каталог для нативных библиотек (LWJGL/JNA): команда ссылается на него через
    // -Djava.library.path и SharedLibraryExtractPath, он должен существовать.
    let _ = fs::create_dir_all(paths.files_root.join("natives"));

    let mut process = Command::new(&command[0]);
    if command.len() > 1 {
        process.args(&command[1..]);
    }
    process.current_dir(&paths.files_root);

    // Гибридная графика (Linux): по умолчанию рендерим на дискретной GPU.
    // detect_offload возвращает что-то только на гибридных системах с применимым
    // offload (NVIDIA с проприетарным драйвером или AMD/Intel через DRI_PRIME).
    if settings.use_discrete_gpu {
        if let Some(offload) = gpu::detect_offload() {
            for (key, value) in &offload.env {
                process.env(key, value);
            }
        }
    }
    // Иначе java.exe открыл бы собственное консольное окно рядом с игрой.
    hide_console_window(&mut process);

    // Пишем stdout+stderr игры в лог профиля. В GUI-режиме (двойной клик) вывод
    // иначе теряется, и мгновенный краш JVM (битая команда запуска, несовместимый
    // агент) невозможно диагностировать. Best-effort: не смогли создать файл —
    // оставляем унаследованные потоки.
    let log_path = paths.profile_root.join("launch.log");
    if let Ok(log_file) = File::create(&log_path) {
        if let Ok(err_file) = log_file.try_clone() {
            process.stdout(Stdio::from(log_file));
            process.stderr(Stdio::from(err_file));
        }
    }

    let started = Instant::now();
    let mut child = process
        .spawn()
        .map_err(|err| format!("Не удалось запустить Minecraft: {}", err))?;

    post_game_started(app);
    discord_rpc::rpc_set(discord_rpc::Presence::Playing {
        nick: nick.to_string(),
    });

    // Keepalive: пока игра запущена, держим yggdrasil-сессию живой по nonce. Стабильный
    // Rust-процесс лаунчера — надёжный сигнал живости, в отличие от heartbeat-треда агента,
    // который в модовом окружении мог тихо умереть → сессия гасла reaper'ом → честного
    // игрока выкидывало «Недействительной сессией» при реконнекте.
    let keepalive_stop = Arc::new(AtomicBool::new(false));
    let keepalive_handle = {
        let api_url = config.api_url();
        let token = token.to_string();
        let nonce = guard.nonce().to_string();
        let stop = keepalive_stop.clone();
        thread::spawn(move || session_keepalive_loop(&api_url, &token, &nonce, &stop))
    };

    // In-game скан процессов: ловит чит-софт, запущенный уже ПОСЛЕ старта игры (pre-launch
    // скан его не видел). Делит stop-флаг с keepalive — оба гаснут на закрытии игры.
    let ingame_scan_handle = anticheat::spawn_ingame_scan(config, &guard, keepalive_stop.clone());

    // Скриншоты по запросу админа теперь снимает НАТИВНЫЙ агент внутри игровой JVM
    // (DXGI/X11), а доставляет Java-агент: лаунчер в этой цепочке больше не участвует —
    // его можно было убить после старта игры, и съёмка умирала.

    let status = child
        .wait()
        .map_err(|err| format!("Не удалось дождаться закрытия Minecraft: {}", err))?;
    let elapsed = started.elapsed();

    // Останавливаем keepalive до инвалидации, чтобы он не продлил уже погашенную сессию.
    keepalive_stop.store(true, Ordering::Relaxed);
    let _ = keepalive_handle.join();
    let _ = ingame_scan_handle.join();
    invalidate_yggdrasil_session(config, &session.access_token);

    // Наиграно: копим только реальные сессии, краш на старте (быстрый выход с
    // ошибкой) не считаем. Best-effort — сбой записи не должен ломать выход из игры.
    if !is_fast_launch_failure(elapsed, status.success()) {
        let _ = update_settings(|settings| {
            settings.played_seconds = settings.played_seconds.saturating_add(elapsed.as_secs());
        });
    }

    // Если античит убил игру (kick-файл создан агентом) — возвращаем уведомление о
    // попытке инжекта вместо обычного сообщения о закрытии.
    if let Some(reason) = guard.finish() {
        return Err(reason.into_alert());
    }

    // Мгновенный выход с ненулевым кодом — это не нормальное закрытие, а краш на
    // старте. Возвращаем ошибку (панель остаётся видимой с путём к логу), иначе
    // краш проглатывается как успех и кнопка «Играть» просто снова становится
    // активной — игра будто «не запустилась».
    if is_fast_launch_failure(elapsed, status.success()) {
        let tail = read_log_tail(&log_path, 20);
        return Err(launch_failure_message(status.code(), &log_path, &tail));
    }

    Ok(minecraft_exit_message(status))
}

/// Порог «мгновенного падения»: нормальный клиент (даже модовый) держит процесс
/// живым дольше при выходе в меню. Быстрый выход игрока завершается кодом 0, поэтому
/// сюда попадает именно краш на старте.
const FAST_LAUNCH_FAILURE: Duration = Duration::from_secs(20);

/// True, если игра завершилась подозрительно быстро и с ошибкой — признак краша на
/// старте (битая команда запуска, отсутствующий мод-загрузчик, несовместимый агент).
fn is_fast_launch_failure(elapsed: Duration, success: bool) -> bool {
    !success && elapsed < FAST_LAUNCH_FAILURE
}

/// Последние `max_lines` непустых строк лога запуска — для показа игроку прямо в
/// сообщении об ошибке без открытия файла. Отсутствие/нечитаемость лога → пустая строка.
fn read_log_tail(path: &Path, max_lines: usize) -> String {
    let content = fs::read_to_string(path).unwrap_or_default();
    let lines: Vec<&str> = content.lines().collect();
    let start = lines.len().saturating_sub(max_lines);
    lines[start..].join("\n").trim().to_string()
}

/// Сообщение об ошибке мгновенного падения: код выхода, путь к полному логу и его хвост.
fn launch_failure_message(code: Option<i32>, log_path: &Path, tail: &str) -> String {
    let code_part = match code {
        Some(c) => format!("код {}", c),
        None => "аварийно (сигнал)".to_string(),
    };
    let mut msg = format!(
        "Minecraft не запустился — процесс завершился сразу ({}). Обычно причина \
         в команде запуска профиля или несовместимом клиенте. Полный лог: {}",
        code_part,
        log_path.display()
    );
    if !tail.is_empty() {
        msg.push_str("\n\nПоследние строки лога:\n");
        msg.push_str(tail);
    }
    msg
}

/// Потолок размера лога ошибок: при превышении лог начинается заново, чтобы
/// бесконечные повторы одной и той же ошибки не съедали диск.
const ERROR_LOG_MAX_BYTES: u64 = 256 * 1024;

/// Unix-время (секунды) → "YYYY-MM-DD HH:MM:SS" в UTC. Календарная часть —
/// алгоритм civil_from_days (Хиннант), без внешних зависимостей.
fn format_log_timestamp(unix_secs: u64) -> String {
    let days = (unix_secs / 86_400) as i64;
    let secs = unix_secs % 86_400;

    let z = days + 719_468;
    let era = z.div_euclid(146_097);
    let doe = z.rem_euclid(146_097);
    let yoe = (doe - doe / 1_460 + doe / 36_524 - doe / 146_096) / 365;
    let doy = doe - (365 * yoe + yoe / 4 - yoe / 100);
    let mp = (5 * doy + 2) / 153;
    let day = doy - (153 * mp + 2) / 5 + 1;
    let month = if mp < 10 { mp + 3 } else { mp - 9 };
    let year = yoe + era * 400 + i64::from(month <= 2);

    format!(
        "{:04}-{:02}-{:02} {:02}:{:02}:{:02}",
        year,
        month,
        day,
        secs / 3_600,
        (secs % 3_600) / 60,
        secs % 60
    )
}

/// Дописывает в лог строку "[<время> UTC] <сообщение>". Создаёт папку при
/// необходимости; разросшийся лог начинает заново (см. ERROR_LOG_MAX_BYTES).
fn append_error_log(path: &Path, unix_secs: u64, message: &str) -> std::io::Result<()> {
    if let Some(parent) = path.parent() {
        fs::create_dir_all(parent)?;
    }
    let oversized = fs::metadata(path)
        .map(|m| m.len() > ERROR_LOG_MAX_BYTES)
        .unwrap_or(false);
    let mut file = fs::OpenOptions::new()
        .create(true)
        .append(!oversized)
        .truncate(oversized)
        .write(true)
        .open(path)?;
    writeln!(
        file,
        "[{} UTC] {}",
        format_log_timestamp(unix_secs),
        message
    )
}

/// Best-effort запись ошибки синхронизации/запуска в постоянный лог
/// (`<data_dir>/sync-errors.log`) — иначе с машины игрока её никак не достать:
/// UI-сообщение живёт до следующего клика, stdout в GUI-режиме не виден.
fn log_sync_error(message: &str) {
    let Ok(dirs) = project_dirs() else { return };
    let secs = SystemTime::now()
        .duration_since(UNIX_EPOCH)
        .map(|elapsed| elapsed.as_secs())
        .unwrap_or(0);
    let _ = append_error_log(&dirs.data_dir().join("sync-errors.log"), secs, message);
}

fn post_game_started(app: &Weak<AppWindow>) {
    let app = app.clone();
    let _ = invoke_from_ui(move || {
        if let Some(app) = app.upgrade() {
            app.set_download_phase("Готово".into());
            app.set_download_file("Minecraft запущен".into());
            app.set_download_counter("100%".into());
            app.set_download_progress(1.0);
            app.set_download_panel_visible(true);
            app.set_message("Minecraft запущен.".into());
            let _ = app.hide();
        }
    });
}

fn minecraft_exit_message(status: ExitStatus) -> String {
    if status.success() {
        return "Minecraft закрыт.".to_string();
    }

    match status.code() {
        Some(code) => format!("Minecraft закрыт с кодом {}.", code),
        None => "Minecraft закрыт.".to_string(),
    }
}

struct PlaceholderValues {
    java: String,
    game_dir: String,
    profile_dir: String,
    login: String,
    uuid: String,
    access_token: String,
    jvm_args: Vec<String>,
}

fn render_command(template: &str, values: &PlaceholderValues) -> Result<Vec<String>, String> {
    let tokens = split_command(template)?;
    let mut rendered = Vec::new();
    let jvm_args = values.jvm_args.join(" ");
    for token in tokens {
        if token == "{jvm_args}" {
            rendered.extend(values.jvm_args.clone());
            continue;
        }
        let token = token
            .replace("{java}", &values.java)
            .replace("{game_dir}", &values.game_dir)
            .replace("{profile_dir}", &values.profile_dir)
            .replace("{login}", &values.login)
            .replace("{uuid}", &values.uuid)
            .replace("{access_token}", &values.access_token)
            .replace("{jvm_args}", &jvm_args);
        if !token.is_empty() {
            rendered.push(token);
        }
    }
    Ok(rendered)
}

fn jvm_args_with_memory(input: &str, memory_gb: i32) -> Result<Vec<String>, String> {
    let mut args = split_command(input)?;
    args.retain(|arg| !is_heap_memory_arg(arg));

    let mut result = Vec::with_capacity(args.len() + 1);
    // Абсолютные границы: системный максимум уже применён вызывающим
    // (effective_memory_gb), здесь только страховка от мусорных значений.
    result.push(format!(
        "-Xmx{}G",
        memory_gb.clamp(MIN_MEMORY_GB, MAX_MEMORY_GB)
    ));
    result.extend(args);
    Ok(result)
}

fn is_heap_memory_arg(arg: &str) -> bool {
    let normalized = arg.to_ascii_lowercase();
    normalized == "-xmx"
        || normalized == "-xms"
        || normalized.starts_with("-xmx")
        || normalized.starts_with("-xms")
}

fn remove_module_path_entries_from_classpath(command: &mut [String]) {
    let separator = classpath_separator();
    let module_entries = module_path_entries(command, separator);
    if module_entries.is_empty() {
        return;
    }

    let mut index = 0;
    while index < command.len() {
        let token = command[index].as_str();
        match token {
            "-cp" | "-classpath" | "--class-path" => {
                if index + 1 < command.len() {
                    command[index + 1] =
                        filter_classpath(&command[index + 1], separator, &module_entries);
                    index += 1;
                }
            }
            _ if token.starts_with("--class-path=") => {
                let classpath = token.trim_start_matches("--class-path=");
                command[index] = format!(
                    "--class-path={}",
                    filter_classpath(classpath, separator, &module_entries)
                );
            }
            _ => {}
        }
        index += 1;
    }
}

fn module_path_entries(command: &[String], separator: char) -> HashSet<String> {
    let mut entries = HashSet::new();
    let mut index = 0;
    while index < command.len() {
        let token = command[index].as_str();
        let module_path = match token {
            "-p" | "--module-path" => {
                if index + 1 >= command.len() {
                    index += 1;
                    continue;
                }
                index += 1;
                command[index].as_str()
            }
            _ if token.starts_with("--module-path=") => token.trim_start_matches("--module-path="),
            _ => {
                index += 1;
                continue;
            }
        };

        for entry in module_path.split(separator) {
            let normalized = normalize_classpath_entry(entry);
            if !normalized.is_empty() {
                entries.insert(normalized);
            }
        }
        index += 1;
    }
    entries
}

fn filter_classpath(classpath: &str, separator: char, excluded: &HashSet<String>) -> String {
    classpath
        .split(separator)
        .filter(|entry| !excluded.contains(&normalize_classpath_entry(entry)))
        .collect::<Vec<_>>()
        .join(&separator.to_string())
}

fn normalize_classpath_entry(entry: &str) -> String {
    let mut normalized = entry
        .trim()
        .trim_matches('"')
        .trim_matches('\'')
        .replace('\\', "/");
    while normalized.starts_with("./") {
        normalized = normalized.trim_start_matches("./").to_string();
    }
    if let Some(index) = normalized.find("libraries/") {
        normalized = normalized[index..].to_string();
    }
    normalized
}

fn classpath_separator() -> char {
    if cfg!(windows) {
        ';'
    } else {
        ':'
    }
}

fn split_command(input: &str) -> Result<Vec<String>, String> {
    let mut result = Vec::new();
    let mut current = String::new();
    let mut quote: Option<char> = None;
    let mut escaped = false;

    for ch in input.chars() {
        if escaped {
            current.push(ch);
            escaped = false;
            continue;
        }
        if ch == '\\' && quote != Some('\'') {
            escaped = true;
            continue;
        }
        if let Some(quote_char) = quote {
            if ch == quote_char {
                quote = None;
            } else {
                current.push(ch);
            }
            continue;
        }
        if ch == '"' || ch == '\'' {
            quote = Some(ch);
            continue;
        }
        if ch.is_whitespace() {
            if !current.is_empty() {
                result.push(current.clone());
                current.clear();
            }
            continue;
        }
        current.push(ch);
    }

    if escaped {
        current.push('\\');
    }
    if quote.is_some() {
        return Err("В команде запуска не закрыта кавычка.".to_string());
    }
    if !current.is_empty() {
        result.push(current);
    }
    Ok(result)
}

fn choose_profile_for_user(
    user_uuid: &str,
    profiles: &[ProfileSummary],
) -> Result<Option<String>, String> {
    let mut selected = None;
    update_settings(|settings| {
        settings.last_user_uuid = Some(user_uuid.to_string());
        selected = settings
            .selected_profiles
            .get(user_uuid)
            .and_then(|profile_id| profiles.iter().find(|profile| &profile.id == profile_id))
            .or_else(|| profiles.iter().find(|profile| profile.is_active))
            .or_else(|| profiles.first())
            .map(|profile| profile.id.clone());

        if let Some(profile_id) = &selected {
            settings
                .selected_profiles
                .insert(user_uuid.to_string(), profile_id.clone());
        }
    })?;
    Ok(selected)
}

fn selected_profile(state: &RuntimeState) -> Option<ProfileSummary> {
    state
        .selected_profile_id
        .as_ref()
        .and_then(|id| state.profiles.iter().find(|profile| &profile.id == id))
        .cloned()
        .or_else(|| state.profiles.first().cloned())
}

/// Применять ли результат пинга. `forced` — игрок сам выбрал пункт «Авто» (он доступен
/// только из настроек, т.е. уже под входом), это команда, а не фоновая инициатива.
/// Фоновый пинг живую сессию не трогает: сессия и yggdrasil живут на том бэкенде, где
/// начался вход. Восстановление сессии (`is_loading`) здесь НЕ учитывается: на медленном
/// основном адресе оно висит до 30с, пинг финиширует раньше и выбор молча терялся.
fn auto_switch_allowed(_forced: bool, is_authenticated: bool) -> bool {
    !is_authenticated
}

/// Пинг серверов в фоне + авто-выбор самого быстрого. Подписи в списке обновляются
/// по мере ответов (`set_row_data` на месте — замена модели сбросила бы выбор игрока),
/// а в режиме «Авто» лаунчер сам переключается на самый быстрый доступный адрес.
///
/// Пингуем ПАРАЛЛЕЛЬНО: последовательный обход держал бы выбор на таймаут каждого
/// мёртвого зеркала (5с × число зеркал).
fn spawn_mirror_probe(
    app_weak: Weak<AppWindow>,
    config: AppConfig,
    mirrors: Vec<(String, String)>,
    forced: bool,
) {
    if mirrors.len() < 2 {
        return; // выбирать не из чего
    }
    thread::spawn(move || {
        let pings = probe_mirrors(&mirrors);
        update_mirror_labels(&app_weak, &mirrors, &pings);

        // Ручной выбор игрока не перебиваем.
        if load_settings().unwrap_or_default().api_url.is_some() {
            return;
        }
        let Some(best) = best_ping_index(&pings) else {
            // Не ответил никто — остаёмся на текущем адресе. При ручном «Авто» об этом
            // надо сказать: иначе висит «Сервер выбирается автоматически…» навсегда.
            if forced {
                let _ = app_weak.upgrade_in_event_loop(|app| {
                    app.set_message("Ни один сервер не ответил.".into());
                });
            }
            return;
        };
        let (name, url) = mirrors[best].clone();
        let ms = pings[best].unwrap_or(0);
        let _ = app_weak.upgrade_in_event_loop(move |app| {
            if !auto_switch_allowed(forced, app.get_is_authenticated()) {
                return;
            }
            config.set_api_url(&url);
            app.set_api_url(url.into());
            app.set_server_name(0, format!("{AUTO_SERVER_LABEL}: {name}"));
            app.set_message(format!("Сервер выбран автоматически: {name} — {ms} мс").into());
        });
    });
}

/// Выбор сервера (окно входа и настройки — один и тот же список). Пункт 0 — «Авто»:
/// сбрасывает ручной выбор и заново ищет самый быстрый. Остальные переключают общий
/// `api_url` (его читают все запросы и фоновые треды) и запоминаются до смены.
fn register_mirror_handler(
    app: &AppWindow,
    config: AppConfig,
    mirrors: Vec<(String, String)>,
    current: usize,
    probe_on_start: bool,
) {
    app.set_server_names(server_items(&mirrors));
    app.set_server_index(current as i32);
    // Селектор показываем, только когда есть из чего выбирать (зеркал больше одного).
    app.set_server_visible(mirrors.len() > 1);
    if probe_on_start {
        spawn_mirror_probe(app.as_weak(), config.clone(), mirrors.clone(), false);
    }

    let app_weak = app.as_weak();
    app.on_server_selected(move |index| {
        let idx = index.max(0) as usize;
        if idx == 0 {
            let _ = update_settings(|settings| settings.api_url = None);
            if let Some(app) = app_weak.upgrade() {
                if app.get_is_authenticated() {
                    app.set_message(
                        "Автовыбор сервера применится после выхода из аккаунта.".into(),
                    );
                    return;
                }
                app.set_server_index(0);
                app.set_message("Сервер выбирается автоматически…".into());
            }
            spawn_mirror_probe(app_weak.clone(), config.clone(), mirrors.clone(), true);
            return;
        }
        let Some((name, url)) = mirrors.get(idx - 1) else {
            return;
        };
        let saved_url = url.clone();
        let _ = update_settings(|settings| settings.api_url = Some(saved_url));
        if let Some(app) = app_weak.upgrade() {
            if app.get_is_authenticated() {
                app.set_message(
                    format!("Сервер {name} будет использован после выхода из аккаунта.").into(),
                );
                return;
            }
            app.set_server_index(idx as i32);
            config.set_api_url(url);
            app.set_api_url(url.into());
            app.set_message(format!("Сервер: {name}").into());
        }
    });
}

fn apply_launcher_settings(app: &AppWindow, settings: &LauncherSettings) {
    let memory_gb = effective_memory_gb(settings);
    app.set_memory_gb(memory_gb);
    app.set_memory_max(system_max_memory_gb());
    app.set_memory_auto(settings.memory_auto);
    app.set_memory_label(memory_label(settings).into());

    // Дискретная видеокарта: секция видна только если на этой системе доступен
    // offload (гибридная графика с применимым драйвером). detect_offload — Linux-only.
    match gpu::detect_offload() {
        Some(offload) => {
            app.set_discrete_gpu_available(true);
            app.set_discrete_gpu_label(offload.vendor_label.into());
        }
        None => app.set_discrete_gpu_available(false),
    }
    app.set_use_discrete_gpu(settings.use_discrete_gpu);
    app.set_discord_rpc_enabled(settings.discord_rpc_enabled);
    app.set_playtime_total(format_playtime(settings.played_seconds).into());
}

/// Наигранное время в человекочитаемом виде: "12 ч 34 мин", "45 мин", "< 1 мин".
fn format_playtime(secs: u64) -> String {
    let hours = secs / 3_600;
    let minutes = (secs % 3_600) / 60;
    match (hours, minutes) {
        (0, 0) => "< 1 мин".to_string(),
        (0, m) => format!("{} мин", m),
        (h, 0) => format!("{} ч", h),
        (h, m) => format!("{} ч {} мин", h, m),
    }
}

// «2026-07-23T10:11:12+03:00» → «2026-07-23 10:11» для карточки аккаунта.
fn format_session_expiry(raw: &str) -> String {
    match raw.split_once('T') {
        Some((date, time)) => format!("{} {}", date, time.get(..5).unwrap_or("")),
        None => raw.to_string(),
    }
}

fn apply_install_folder_label(app: &AppWindow) {
    let folder = current_install_root()
        .or_else(|_| project_dirs().map(|dirs| dirs.data_dir().to_path_buf()));

    if let Ok(folder) = folder {
        app.set_install_folder(folder.to_string_lossy().to_string().into());
    }
}

fn update_memory_settings<F>(app: &Weak<AppWindow>, update: F)
where
    F: FnOnce(&mut LauncherSettings),
{
    if let Some(app) = app.upgrade() {
        match update_settings(|settings| {
            update(settings);
            settings.memory_gb = clamp_memory_gb(settings.memory_gb);
        }) {
            Ok(settings) => {
                apply_launcher_settings(&app, &settings);
                app.set_message("Настройки памяти сохранены.".into());
            }
            Err(message) => app.set_message(message.into()),
        }
    }
}

fn install_folder_for_state(state: &RuntimeState) -> Result<PathBuf, String> {
    if let Some(user) = &state.user {
        if let Some(profile) = selected_profile(state) {
            return Ok(profile_paths(user, &profile.id)?.files_root);
        }
    }
    Ok(project_dirs()?.data_dir().to_path_buf())
}

fn open_folder(path: &Path) -> Result<(), String> {
    let mut command = if cfg!(target_os = "windows") {
        let mut command = Command::new("explorer");
        command.arg(path);
        command
    } else if cfg!(target_os = "macos") {
        let mut command = Command::new("open");
        command.arg(path);
        command
    } else {
        let mut command = Command::new("xdg-open");
        command.arg(path);
        command
    };

    command
        .spawn()
        .map(|_| ())
        .map_err(|err| format!("Не удалось открыть папку: {}", err))
}

fn memory_label(settings: &LauncherSettings) -> String {
    let memory_gb = effective_memory_gb(settings);
    if settings.memory_auto {
        format!("Авто · {} ГБ", memory_gb)
    } else {
        format!("{} ГБ", memory_gb)
    }
}

fn effective_memory_gb(settings: &LauncherSettings) -> i32 {
    effective_memory_capped(settings, system_max_memory_gb())
}

/// Чистая версия для тестов: авто-дефолт + зажим в [MIN_MEMORY_GB, max_gb].
fn effective_memory_capped(settings: &LauncherSettings, max_gb: i32) -> i32 {
    let memory_gb = if settings.memory_auto {
        DEFAULT_MEMORY_GB
    } else {
        settings.memory_gb
    };
    memory_gb.clamp(MIN_MEMORY_GB, max_gb.max(MIN_MEMORY_GB))
}

fn clamp_memory_gb(value: i32) -> i32 {
    value.clamp(MIN_MEMORY_GB, system_max_memory_gb())
}

/// Максимум слайдера памяти: физическое ОЗУ машины, зажатое в
/// [MIN_MEMORY_GB, MAX_MEMORY_GB]. Кешируется на процесс.
fn system_max_memory_gb() -> i32 {
    static MAX: OnceLock<i32> = OnceLock::new();
    *MAX.get_or_init(|| {
        detect_total_memory_gb()
            .unwrap_or(MAX_MEMORY_GB)
            .clamp(MIN_MEMORY_GB, MAX_MEMORY_GB)
    })
}

/// Общий объём ОЗУ машины в ГБ (округление к ближайшему гигабайту).
/// None — определить не удалось (тогда остаётся старый максимум 64 ГБ).
fn detect_total_memory_gb() -> Option<i32> {
    #[cfg(target_os = "linux")]
    {
        let meminfo = fs::read_to_string("/proc/meminfo").ok()?;
        let kb: i64 = meminfo
            .lines()
            .find_map(|line| line.strip_prefix("MemTotal:"))?
            .trim()
            .trim_end_matches("kB")
            .trim()
            .parse()
            .ok()?;
        // кБ → ГБ с округлением к ближайшему (1 << 19 кБ = 0.5 ГБ).
        return Some((((kb + (1 << 19)) >> 20) as i32).max(1));
    }
    #[cfg(windows)]
    {
        // GlobalMemoryStatusEx из kernel32 — без сторонних зависимостей.
        #[repr(C)]
        struct MemoryStatusEx {
            length: u32,
            memory_load: u32,
            total_phys: u64,
            avail_phys: u64,
            total_page_file: u64,
            avail_page_file: u64,
            total_virtual: u64,
            avail_virtual: u64,
            avail_extended_virtual: u64,
        }
        #[link(name = "kernel32")]
        extern "system" {
            fn GlobalMemoryStatusEx(buffer: *mut MemoryStatusEx) -> i32;
        }
        let mut status = MemoryStatusEx {
            length: std::mem::size_of::<MemoryStatusEx>() as u32,
            memory_load: 0,
            total_phys: 0,
            avail_phys: 0,
            total_page_file: 0,
            avail_page_file: 0,
            total_virtual: 0,
            avail_virtual: 0,
            avail_extended_virtual: 0,
        };
        if unsafe { GlobalMemoryStatusEx(&mut status) } == 0 {
            return None;
        }
        // байты → МБ → ГБ с округлением к ближайшему.
        let mb = status.total_phys >> 20;
        return Some((((mb + 512) >> 10) as i32).max(1));
    }
    #[allow(unreachable_code)]
    None
}

fn default_memory_gb() -> i32 {
    DEFAULT_MEMORY_GB
}

fn default_memory_auto() -> bool {
    true
}

fn default_use_discrete_gpu() -> bool {
    true
}

fn default_discord_rpc_enabled() -> bool {
    true
}

fn post_progress(
    app: &Weak<AppWindow>,
    phase: &str,
    file_name: &str,
    counter: &str,
    progress: f32,
    visible: bool,
) {
    let app = app.clone();
    let phase = phase.to_string();
    let file_name = file_name.to_string();
    let counter = counter.to_string();
    let progress = progress.clamp(0.0, 1.0);
    let _ = invoke_from_ui(move || {
        if let Some(app) = app.upgrade() {
            app.set_download_phase(phase.into());
            app.set_download_file(file_name.into());
            app.set_download_counter(counter.into());
            app.set_download_progress(progress);
            app.set_download_panel_visible(visible);
        }
    });
}

/// Билдер HTTP-клиента для ВСЕХ вызовов своего бэкенда/античита (не для сторонних хостов).
/// Защита от HTTP-дебагеров/перехватчиков (Fiddler, Charles, Burp, mitmproxy):
///  - `use_rustls_tls()` + вшитые webpki-roots вместо системного хранилища сертификатов:
///    корневой CA перехватчика, установленный в ОС, НЕ доверенный → TLS-handshake падает,
///    трафик авторизации/античита нельзя ни расшифровать, ни подменить.
///  - `no_proxy()`: игнорируем HTTP(S)_PROXY/ALL_PROXY, чтобы не ходить через прокси-перехватчик.
///
/// Прод-домены (launcher/mirror.likonchik.xyz) выпущены Let's Encrypt (ISRG Root X1 есть в
/// webpki-roots), поэтому канал работает. ⚠️ Смена CA бэкенда на корень вне webpki потребует
/// пересборки лаунчера (роутинг сертификатов вшит в бинарник). Сторонние загрузки (Mojang/S3)
/// намеренно остаются на системном хранилище + системном прокси — см. download_client.
pub(crate) fn hardened_backend_builder() -> reqwest::blocking::ClientBuilder {
    Client::builder()
        .use_rustls_tls()
        .no_proxy()
        .gzip(true)
        .zstd(true)
}

static API_CLIENT: LazyLock<Result<Client, String>> = LazyLock::new(|| {
    hardened_backend_builder()
        .timeout(Duration::from_secs(30))
        .build()
        .map_err(|_| "Не удалось создать HTTP клиент.".to_string())
});

fn http_client() -> Result<Client, String> {
    API_CLIENT
        .as_ref()
        .map(Client::clone)
        .map_err(String::clone)
}

#[cfg(test)]
#[test]
fn hardened_client_negotiates_and_decodes_zstd() {
    use std::io::{Read, Write};
    use std::net::TcpListener;

    let listener = TcpListener::bind("127.0.0.1:0").expect("bind test server");
    let address = listener.local_addr().expect("test server address");
    let server = std::thread::spawn(move || {
        let (mut stream, _) = listener.accept().expect("accept client");
        let mut request = [0_u8; 8_192];
        let read = stream.read(&mut request).expect("read request");
        let headers = String::from_utf8_lossy(&request[..read]).to_lowercase();
        assert!(headers.contains("accept-encoding:"));
        assert!(headers.contains("zstd"));

        let body = zstd::stream::encode_all(b"{\"manifest\":true}".as_slice(), 1)
            .expect("encode zstd response");
        write!(
            stream,
            "HTTP/1.1 200 OK\r\nContent-Encoding: zstd\r\nContent-Length: {}\r\nConnection: close\r\n\r\n",
            body.len()
        )
        .expect("write response headers");
        stream.write_all(&body).expect("write response body");
    });

    let response = hardened_backend_builder()
        .build()
        .expect("build hardened client")
        .get(format!("http://{address}/manifest"))
        .send()
        .expect("request compressed manifest")
        .text()
        .expect("decode compressed manifest");
    assert_eq!(response, "{\"manifest\":true}");
    server.join().expect("test server");
}

/// Клиент для скачивания СТОРОННИХ файлов (Mojang java-runtime, S3-зеркало профилей):
/// системное хранилище CA + системный прокси (их сертификаты могут быть вне webpki, а
/// целостность и так гарантируется SHA-сверкой). Без общего таймаута (большие файлы).
fn download_client() -> Result<Client, String> {
    Client::builder()
        .connect_timeout(Duration::from_secs(15))
        .tcp_keepalive(Duration::from_secs(20))
        .build()
        .map_err(|_| "Не удалось создать HTTP клиент.".to_string())
}

/// Клиент для скачивания С БЭКЕНДА (манифест античита, agent.jar/native, authlib,
/// файлы профиля с api_url): как download_client, но по защищённому каналу
/// (rustls+webpki, без прокси) — от HTTP-перехватчиков.
fn backend_download_client() -> Result<Client, String> {
    hardened_backend_builder()
        .connect_timeout(Duration::from_secs(15))
        .tcp_keepalive(Duration::from_secs(20))
        .build()
        .map_err(|_| "Не удалось создать HTTP клиент.".to_string())
}

fn normalize_totp_code(value: &str) -> String {
    value
        .chars()
        .filter(|character| !character.is_whitespace())
        .collect()
}

/// Пишет токен в системное хранилище и СВЕРЯЕТ, что он читается оттуда новым `Entry`.
///
/// Верить одному `set_password` нельзя: keyring без фич-бэкендов собирается в заглушку,
/// которая держит секрет внутри самого объекта `Entry` и возвращает Ok — токен уходил
/// «в никуда», а fallback в settings.json при этом обнулялся, и сессия не переживала
/// перезапуск лаунчера. Тот же эффект даёт заблокированный или не запущенный агент
/// хранилища, поэтому проверка нужна и с настоящим бэкендом.
fn keyring_persists(token: &str) -> bool {
    let stored = keyring::Entry::new(KEYRING_SERVICE, KEYRING_USER)
        .and_then(|entry| entry.set_password(token));
    if stored.is_err() {
        return false;
    }
    keyring::Entry::new(KEYRING_SERVICE, KEYRING_USER)
        .and_then(|entry| entry.get_password())
        .is_ok_and(|saved| saved == token)
}

fn save_token(token: &str) -> Result<(), String> {
    let keyring_ok = keyring_persists(token);

    // JWT в settings.json (плейнтекст, права по umask) кладём ТОЛЬКО как fallback,
    // когда keyring недоступен. При рабочем keyring поле очищаем — иначе токен всегда
    // лежал бы открыто рядом, и keyring не давал бы защиты (кража = просто чтение файла).
    let fallback_token = (!keyring_ok).then(|| token.to_string());
    let settings_result = update_settings(|settings| settings.auth_token = fallback_token);

    if keyring_ok || settings_result.is_ok() {
        return Ok(());
    }
    Err(settings_result
        .err()
        .unwrap_or_else(|| "Не удалось сохранить сессию.".to_string()))
}

fn read_token() -> Result<String, String> {
    if let Ok(entry) = keyring::Entry::new(KEYRING_SERVICE, KEYRING_USER) {
        if let Ok(token) = entry.get_password() {
            if !token.trim().is_empty() {
                return Ok(token);
            }
        }
    }

    let settings = load_settings()?;
    settings
        .auth_token
        .filter(|token| !token.trim().is_empty())
        .ok_or_else(|| "Сохранённая сессия не найдена.".to_string())
}

fn delete_token() -> Result<(), String> {
    if let Ok(entry) = keyring::Entry::new(KEYRING_SERVICE, KEYRING_USER) {
        let _ = entry.delete_credential();
    }

    update_settings(|settings| settings.auth_token = None).map(|_| ())
}

fn should_forget_saved_session(message: &str) -> bool {
    let normalized = message.to_lowercase();
    normalized.contains("http 401")
        || normalized.contains("требуется авторизация")
        || normalized.contains("сессия недействительна")
        || normalized.contains("unauthorized")
}

fn load_settings() -> Result<LauncherSettings, String> {
    let _guard = SETTINGS_IO_LOCK
        .lock()
        .map_err(|_| "Не удалось заблокировать settings.json.".to_string())?;
    load_settings_unlocked()
}

fn load_settings_unlocked() -> Result<LauncherSettings, String> {
    let path = settings_path()?;
    let pending = path.with_extension("json.new");
    let backup = path.with_extension("json.old");
    if !path.exists() {
        for candidate in [&pending, &backup] {
            if !candidate.is_file() {
                continue;
            }
            let data = fs::read_to_string(candidate)
                .map_err(|_| "Не удалось восстановить settings.json.".to_string())?;
            if serde_json::from_str::<LauncherSettings>(&data).is_ok() {
                fs::rename(candidate, &path)
                    .map_err(|_| "Не удалось восстановить settings.json.".to_string())?;
                break;
            }
            let _ = fs::remove_file(candidate);
        }
        if !path.exists() {
            return Ok(LauncherSettings::default());
        }
    }
    let data =
        fs::read_to_string(&path).map_err(|_| "Не удалось прочитать settings.json.".to_string())?;
    let settings =
        serde_json::from_str(&data).map_err(|_| "settings.json повреждён.".to_string())?;
    let _ = fs::remove_file(pending);
    let _ = fs::remove_file(backup);
    Ok(settings)
}

fn save_settings_unlocked(settings: &LauncherSettings) -> Result<(), String> {
    let path = settings_path()?;
    let parent = path
        .parent()
        .ok_or_else(|| "Не удалось определить папку настроек.".to_string())?;
    fs::create_dir_all(parent).map_err(|_| "Не удалось создать папку настроек.".to_string())?;
    let data = serde_json::to_string_pretty(settings)
        .map_err(|_| "Не удалось сохранить настройки.".to_string())?;
    let pending = path.with_extension("json.new");
    let backup = path.with_extension("json.old");
    let mut file =
        File::create(&pending).map_err(|_| "Не удалось подготовить settings.json.".to_string())?;
    file.write_all(data.as_bytes())
        .and_then(|_| file.flush())
        .and_then(|_| file.sync_all())
        .map_err(|_| "Не удалось записать settings.json.".to_string())?;
    drop(file);

    if backup.exists() {
        fs::remove_file(&backup)
            .map_err(|_| "Не удалось очистить резервную копию настроек.".to_string())?;
    }
    if path.exists() {
        fs::rename(&path, &backup)
            .map_err(|_| "Не удалось подготовить замену settings.json.".to_string())?;
    }
    if let Err(error) = fs::rename(&pending, &path) {
        if backup.exists() && !path.exists() {
            let _ = fs::rename(&backup, &path);
        }
        return Err(format!("Не удалось заменить settings.json: {error}"));
    }
    #[cfg(unix)]
    if let Ok(directory) = File::open(parent) {
        let _ = directory.sync_all();
    }
    let _ = fs::remove_file(backup);
    Ok(())
}

/// Атомарная для потоков лаунчера операция read-modify-write. Все UI-настройки
/// используют её, поэтому фоновый перенос диска не может затереть память/GPU и
/// наоборот. Возвращается сохранённый снимок для немедленного обновления UI.
fn update_settings<F>(update: F) -> Result<LauncherSettings, String>
where
    F: FnOnce(&mut LauncherSettings),
{
    let _guard = SETTINGS_IO_LOCK
        .lock()
        .map_err(|_| "Не удалось заблокировать settings.json.".to_string())?;
    let mut settings = load_settings_unlocked().unwrap_or_default();
    update(&mut settings);
    save_settings_unlocked(&settings)?;
    Ok(settings)
}

fn save_local_manifest(path: &Path, files_root: &Path, manifest: &Manifest) -> Result<(), String> {
    if let Some(parent) = path.parent() {
        fs::create_dir_all(parent).map_err(|_| "Не удалось создать папку manifest.".to_string())?;
    }
    let local = LocalManifest {
        profile_id: manifest.profile.id.clone(),
        manifest_version: manifest.profile.manifest_version,
        files: manifest
            .files
            .iter()
            .map(|file| {
                // Локальный manifest остаётся служебной записью состояния профиля;
                // проверка безопасности всегда заново сверяет SHA256 с backend manifest.
                let mtime_millis = safe_join(files_root, &file.path)
                    .ok()
                    .and_then(|target| fs::metadata(target).ok())
                    .map(|metadata| file_mtime_millis(&metadata))
                    .unwrap_or(0);
                LocalFileRecord {
                    path: file.path.clone(),
                    hash_sha256: file.hash_sha256.clone(),
                    size: file.size,
                    mtime_millis,
                }
            })
            .collect(),
    };
    let data = serde_json::to_string_pretty(&local)
        .map_err(|_| "Не удалось сериализовать локальный manifest.".to_string())?;
    fs::write(path, data).map_err(|_| "Не удалось записать локальный manifest.".to_string())
}

fn project_dirs() -> Result<ProjectDirs, String> {
    ProjectDirs::from("xyz", "", "Project Minecraft")
        .ok_or_else(|| "Не удалось определить папку данных лаунчера.".to_string())
}

fn settings_path() -> Result<PathBuf, String> {
    Ok(project_dirs()?.config_dir().join("settings.json"))
}

fn profile_paths(user: &AuthUser, profile_id: &str) -> Result<ProfilePaths, String> {
    profile_paths_at_root(user, profile_id, &current_install_root()?)
}

fn current_install_root() -> Result<PathBuf, String> {
    let default_root = project_dirs()?.data_dir().to_path_buf();
    let settings = load_settings()?;
    install::configured_root(settings.install_root.as_deref(), &default_root)
}

fn profile_paths_at_root(
    user: &AuthUser,
    profile_id: &str,
    install_root: &Path,
) -> Result<ProfilePaths, String> {
    let user_key = safe_component(if user.provider_uuid.is_empty() {
        &user.id
    } else {
        &user.provider_uuid
    });
    let profile_key = safe_component(profile_id);
    let profile_root = install_root
        .join("users")
        .join(user_key)
        .join("profiles")
        .join(profile_key);
    let files_root = profile_root.join("files");
    let manifest_path = profile_root.join("manifest.json");
    Ok(ProfilePaths {
        profile_root,
        files_root,
        manifest_path,
    })
}

fn safe_component(value: &str) -> String {
    let mut result = String::new();
    for ch in value.chars() {
        if ch.is_ascii_alphanumeric() || ch == '-' || ch == '_' {
            result.push(ch);
        } else {
            result.push('_');
        }
    }
    if result.is_empty() {
        "unknown".to_string()
    } else {
        result
    }
}

fn safe_join(root: &Path, rel: &str) -> Result<PathBuf, String> {
    let mut path = PathBuf::from(root);
    for component in Path::new(rel).components() {
        match component {
            Component::Normal(part) => path.push(part),
            Component::CurDir => {}
            _ => return Err(format!("Небезопасный путь в manifest: {}", rel)),
        }
    }

    let root_abs = root
        .canonicalize()
        .or_else(|_| std::env::current_dir().map(|cwd| cwd.join(root)))
        .map_err(|_| "Не удалось проверить путь профиля.".to_string())?;
    // Сверяем по ближайшему СУЩЕСТВУЮЩЕМУ предку (минимум — сам root). На Windows
    // `canonicalize` несуществующего пути падает, а fallback даёт путь без verbatim
    // префикса `\\?\`, тогда как root_abs — с ним; их `starts_with` всегда давал
    // false и ронял скачивание любого вложенного файла. Канонизация существующего
    // предка приводит обе стороны к одному виду. Лексический выход за root уже
    // исключён циклом выше (только Normal/CurDir), это защита от symlink-escape.
    let mut existing = path.as_path();
    while !existing.exists() {
        match existing.parent() {
            Some(parent) => existing = parent,
            None => break,
        }
    }
    let existing_abs = existing
        .canonicalize()
        .unwrap_or_else(|_| existing.to_path_buf());
    if existing_abs != root_abs && !existing_abs.starts_with(&root_abs) {
        return Err(format!("Путь выходит за папку профиля: {}", rel));
    }
    Ok(path)
}

fn hash_file(path: &Path) -> Result<String, String> {
    let mut file =
        File::open(path).map_err(|_| "Не удалось открыть файл для проверки.".to_string())?;
    let mut hasher = Sha256::new();
    let mut buffer = [0_u8; 64 * 1024];
    loop {
        let read = file
            .read(&mut buffer)
            .map_err(|_| "Не удалось прочитать файл для проверки.".to_string())?;
        if read == 0 {
            break;
        }
        hasher.update(&buffer[..read]);
    }
    Ok(hex_hash(hasher.finalize().as_slice()))
}

fn hash_file_sha1(path: &Path) -> Result<String, String> {
    let mut file =
        File::open(path).map_err(|_| "Не удалось открыть Java файл для проверки.".to_string())?;
    let mut hasher = Sha1::new();
    let mut buffer = [0_u8; 64 * 1024];
    loop {
        let read = file
            .read(&mut buffer)
            .map_err(|_| "Не удалось прочитать Java файл для проверки.".to_string())?;
        if read == 0 {
            break;
        }
        hasher.update(&buffer[..read]);
    }
    Ok(hex_hash(hasher.finalize().as_slice()))
}

fn fetch_sha1_bytes(
    client: &Client,
    endpoint: &str,
    expected_sha1: &str,
    expected_size: i64,
    label: &str,
) -> Result<Vec<u8>, String> {
    let mut response = client
        .get(endpoint)
        .send()
        .map_err(|_| format!("Не удалось скачать {}.", label))?;
    if !response.status().is_success() {
        return Err(format!(
            "Ошибка скачивания {}: HTTP {}",
            label,
            response.status().as_u16()
        ));
    }

    let mut data = Vec::new();
    let mut buffer = [0_u8; 64 * 1024];
    let mut hasher = Sha1::new();
    loop {
        let read = response
            .read(&mut buffer)
            .map_err(|_| format!("Ошибка чтения {}.", label))?;
        if read == 0 {
            break;
        }
        hasher.update(&buffer[..read]);
        data.extend_from_slice(&buffer[..read]);
    }

    let hash = hex_hash(hasher.finalize().as_slice());
    if !expected_sha1.is_empty() && hash != expected_sha1.to_lowercase() {
        return Err(format!("Hash mismatch: {}.", label));
    }
    if expected_size >= 0 && data.len() != expected_size as usize {
        return Err(format!("Размер {} изменился.", label));
    }
    Ok(data)
}

fn ensure_executable(
    #[cfg_attr(not(unix), allow(unused_variables))] path: &Path,
    executable: bool,
) -> Result<(), String> {
    if !executable {
        return Ok(());
    }
    #[cfg(unix)]
    {
        use std::os::unix::fs::PermissionsExt;

        let metadata =
            fs::metadata(path).map_err(|_| "Не удалось проверить Java executable.".to_string())?;
        let mut permissions = metadata.permissions();
        permissions.set_mode(permissions.mode() | 0o755);
        fs::set_permissions(path, permissions)
            .map_err(|_| "Не удалось выставить права запуска для Java.".to_string())?;
    }
    Ok(())
}

fn set_manifest_executable(
    #[cfg_attr(not(unix), allow(unused_variables))] path: &Path,
    executable: bool,
) -> Result<(), String> {
    #[cfg(unix)]
    {
        use std::os::unix::fs::PermissionsExt;

        let metadata = fs::metadata(path)
            .map_err(|_| "Не удалось проверить права файла сборки.".to_string())?;
        let mut permissions = metadata.permissions();
        let mode = if executable {
            permissions.mode() | 0o111
        } else {
            permissions.mode() & !0o111
        };
        permissions.set_mode(mode);
        fs::set_permissions(path, permissions)
            .map_err(|_| "Не удалось выставить права файла сборки.".to_string())?;
    }
    Ok(())
}

fn create_java_link(root: &Path, rel: &str, target: &str) -> Result<(), String> {
    if target.trim().is_empty() {
        return Ok(());
    }
    let link_path = safe_join(root, rel)?;
    if let Some(parent) = link_path.parent() {
        fs::create_dir_all(parent)
            .map_err(|_| format!("Не удалось создать папку Java link: {}", rel))?;
    }
    remove_existing_path_for_replace(&link_path)
        .map_err(|_| format!("Не удалось заменить Java link: {}", rel))?;

    #[cfg(unix)]
    {
        std::os::unix::fs::symlink(target, &link_path)
            .map_err(|_| format!("Не удалось создать Java link: {}", rel))?;
    }

    Ok(())
}

fn java_runtime_platform_key() -> &'static str {
    if cfg!(target_os = "windows") {
        if cfg!(target_arch = "aarch64") {
            "windows-arm64"
        } else if cfg!(target_arch = "x86") {
            "windows-x86"
        } else {
            "windows-x64"
        }
    } else if cfg!(target_os = "macos") {
        if cfg!(target_arch = "aarch64") {
            "mac-os-arm64"
        } else {
            "mac-os"
        }
    } else {
        "linux"
    }
}

fn java_runtime_component(java_version: i32) -> &'static str {
    let java_version = if java_version <= 0 { 17 } else { java_version };
    match java_version {
        version if version >= 25 => "java-runtime-epsilon",
        version if version >= 21 => "java-runtime-delta",
        17..=20 => "java-runtime-gamma",
        16 => "java-runtime-alpha",
        _ => "jre-legacy",
    }
}

fn java_runtime_executable_rel(platform_key: &str) -> &'static str {
    if platform_key.starts_with("windows") {
        "bin/java.exe"
    } else if platform_key.starts_with("mac-os") {
        "jre.bundle/Contents/Home/bin/java"
    } else {
        "bin/java"
    }
}

fn java_runtime_root_rel(java_rel: &str, executable_rel: &str) -> Result<String, String> {
    let normalized = java_rel.replace('\\', "/");
    if let Some(prefix) = normalized.strip_suffix(executable_rel) {
        return Ok(prefix.trim_end_matches('/').to_string());
    }

    let path = Path::new(&normalized);
    let bin_dir = path
        .parent()
        .ok_or_else(|| "Путь Java runtime должен вести к bin/java.".to_string())?;
    let root = bin_dir
        .parent()
        .ok_or_else(|| "Путь Java runtime должен лежать внутри runtime/<os>.".to_string())?;
    Ok(root.to_string_lossy().replace('\\', "/"))
}

fn java_runtime_managed_paths(root_rel: &str, manifest: &JavaRuntimeManifest) -> HashSet<String> {
    let mut paths = HashSet::new();
    let root_rel = normalize_relative_path(root_rel).unwrap_or_default();
    for (path, entry) in &manifest.files {
        if entry.kind == "directory" {
            continue;
        }
        let joined = join_relative_path(&root_rel, path);
        if let Some(path) = normalize_relative_path(&joined) {
            paths.insert(path);
        }
    }
    paths
}

fn ensure_directory(path: &Path, message: &str) -> Result<(), String> {
    match fs::symlink_metadata(path) {
        Ok(metadata) if metadata.is_dir() && !metadata.file_type().is_symlink() => Ok(()),
        Ok(_) => {
            remove_existing_path_for_replace(path).map_err(|_| message.to_string())?;
            fs::create_dir_all(path).map_err(|_| message.to_string())
        }
        Err(error) if error.kind() == std::io::ErrorKind::NotFound => {
            fs::create_dir_all(path).map_err(|_| message.to_string())
        }
        Err(_) => Err(message.to_string()),
    }
}

fn remove_existing_path_for_replace(path: &Path) -> std::io::Result<()> {
    let metadata = match fs::symlink_metadata(path) {
        Ok(metadata) => metadata,
        Err(error) if error.kind() == std::io::ErrorKind::NotFound => return Ok(()),
        Err(error) => return Err(error),
    };

    if metadata.is_dir() && !metadata.file_type().is_symlink() {
        fs::remove_dir_all(path)
    } else {
        fs::remove_file(path)
    }
}

fn relative_path(root: &Path, path: &Path) -> Result<String, String> {
    let rel = path
        .strip_prefix(root)
        .map_err(|_| "Путь выходит за папку профиля.".to_string())?;
    let mut parts = Vec::new();
    for component in rel.components() {
        match component {
            Component::Normal(part) => {
                let part = part
                    .to_str()
                    .ok_or_else(|| "Путь профиля содержит некорректный UTF-8.".to_string())?;
                parts.push(part.to_string());
            }
            Component::CurDir => {}
            _ => return Err("Путь выходит за папку профиля.".to_string()),
        }
    }
    Ok(parts.join("/"))
}

fn normalize_relative_path(value: &str) -> Option<String> {
    let value = value.trim().replace('\\', "/");
    if value.is_empty() || value.starts_with('/') || value.contains(':') {
        return None;
    }

    let mut parts = Vec::new();
    for component in Path::new(&value).components() {
        match component {
            Component::Normal(part) => parts.push(part.to_str()?.to_string()),
            Component::CurDir => {}
            _ => return None,
        }
    }
    if parts.is_empty() {
        None
    } else {
        Some(parts.join("/"))
    }
}

fn normalize_preserve_paths(paths: &[String]) -> Vec<String> {
    let mut seen = HashSet::new();
    let mut result = Vec::new();
    for path in paths {
        let Some(path) = normalize_preserve_path(path) else {
            continue;
        };
        if seen.insert(path.clone()) {
            result.push(path);
        }
    }
    result
}

fn normalize_preserve_path(value: &str) -> Option<String> {
    let value = value.trim().replace('\\', "/");
    if value.is_empty() || value.starts_with('/') || value.contains(':') {
        return None;
    }
    let is_dir = value.ends_with('/');
    let mut normalized = normalize_relative_path(value.trim_end_matches('/'))?;
    if is_reserved_preserve_path(&normalized) {
        return None;
    }
    if is_dir {
        normalized.push('/');
    }
    Some(normalized)
}

fn is_reserved_preserve_path(path: &str) -> bool {
    let root = path
        .trim_end_matches('/')
        .split('/')
        .next()
        .unwrap_or_default();
    matches!(
        root,
        "mods" | "libraries" | "versions" | "assets" | "runtime"
    )
}

fn preserve_path_matches(rel: &str, preserve_paths: &[String]) -> bool {
    for preserve_path in preserve_paths {
        if preserve_path.ends_with('/') {
            let root = preserve_path.trim_end_matches('/');
            if rel == root || rel.starts_with(preserve_path) {
                return true;
            }
            continue;
        }
        if rel == preserve_path {
            return true;
        }
    }
    false
}

fn join_relative_path(base: &str, rel: &str) -> String {
    let rel = rel.trim_start_matches('/').replace('\\', "/");
    if base.is_empty() {
        rel
    } else if rel.is_empty() {
        base.to_string()
    } else {
        format!("{}/{}", base.trim_end_matches('/'), rel)
    }
}

fn hex_hash(bytes: &[u8]) -> String {
    let mut result = String::with_capacity(bytes.len() * 2);
    for byte in bytes {
        result.push_str(&format!("{:02x}", byte));
    }
    result
}

fn temp_download_path(target: &Path) -> PathBuf {
    let file_name = target
        .file_name()
        .and_then(|value| value.to_str())
        .unwrap_or("download");
    target.with_file_name(format!(".{}.download", file_name))
}

fn absolute_api_url(config: &AppConfig, value: &str) -> String {
    if value.starts_with("http://") || value.starts_with("https://") {
        value.to_string()
    } else {
        format!(
            "{}/{}",
            config.api_url().trim_end_matches('/'),
            value.trim_start_matches('/')
        )
    }
}

/// Ведёт ли absolute-URL на выбранный бэкенд (а не на публичное зеркало файлов).
/// Граница — `/` после базы: без неё `https://api.example.com.evil.tld` прошёл бы
/// как «свой» и получил бы JWT игрока.
fn is_api_url(config: &AppConfig, url: &str) -> bool {
    let api_url = config.api_url();
    let base = api_url.trim_end_matches('/');
    url.strip_prefix(base)
        .is_some_and(|rest| rest.is_empty() || rest.starts_with('/'))
}

fn os_value<'a>(windows: &'a str, linux: &'a str, macos: &'a str) -> &'a str {
    if cfg!(target_os = "windows") {
        windows
    } else if cfg!(target_os = "macos") {
        macos
    } else {
        linux
    }
}

fn format_bytes(value: i64) -> String {
    if value <= 0 {
        return "0 B".to_string();
    }
    let units = ["B", "KB", "MB", "GB"];
    let mut amount = value as f64;
    let mut unit_index = 0_usize;
    while amount >= 1024.0 && unit_index < units.len() - 1 {
        amount /= 1024.0;
        unit_index += 1;
    }
    if unit_index == 0 {
        format!("{} {}", amount as i64, units[unit_index])
    } else {
        format!("{:.1} {}", amount, units[unit_index])
    }
}

impl LoginError {
    fn unavailable() -> Self {
        Self {
            message: "Backend лаунчера недоступен.".to_string(),
            requires_two_factor: false,
        }
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    // Сетевой смоук: защищённый клиент (rustls+webpki, без прокси) ДОЛЖЕН достучаться до
    // прод-бэкенда и зеркала (их LE-сертификаты в webpki-roots), иначе логин кирпич.
    // Не в обычном прогоне: cargo test hardened_client_reaches_prod -- --ignored
    #[test]
    #[ignore]
    fn hardened_client_reaches_prod() {
        let client = hardened_backend_builder()
            .timeout(Duration::from_secs(20))
            .build()
            .expect("build hardened client");
        for host in [
            "https://launcher.likonchik.xyz",
            "https://mirror.likonchik.xyz",
        ] {
            let resp = client
                .get(format!("{host}/api/policy"))
                .send()
                .unwrap_or_else(|e| panic!("{host}: {e}"));
            assert!(resp.status().is_success(), "{host}: HTTP {}", resp.status());
        }
    }

    #[test]
    fn bearer_goes_only_to_own_backend() {
        let config = AppConfig {
            api_url: Arc::new(RwLock::new("https://launcher.likonchik.xyz".to_string())),
        };

        assert!(is_api_url(
            &config,
            "https://launcher.likonchik.xyz/api/profiles/1/files/mods/a.jar"
        ));
        assert!(is_api_url(&config, "https://launcher.likonchik.xyz"));
        // Файлы с бакета — без токена.
        assert!(!is_api_url(
            &config,
            "https://pjm-files.s3.cloud.ru/vanilla/files/mods/a.jar"
        ));
        // Префикс базы, но чужой хост — токен туда уйти не должен.
        assert!(!is_api_url(
            &config,
            "https://launcher.likonchik.xyz.evil.tld/api/steal"
        ));
    }

    #[test]
    fn playtime_formats_by_magnitude() {
        assert_eq!(format_playtime(0), "< 1 мин");
        assert_eq!(format_playtime(59), "< 1 мин");
        assert_eq!(format_playtime(45 * 60), "45 мин");
        assert_eq!(format_playtime(3_600), "1 ч");
        assert_eq!(format_playtime(12 * 3_600 + 34 * 60), "12 ч 34 мин");
    }

    #[test]
    fn auto_switch_applies_unless_session_is_live() {
        assert!(auto_switch_allowed(false, false));
        // Ни фон, ни ручной выбор из настроек не меняют backend живой сессии.
        // Выбор сохраняется и применяется после logout.
        assert!(!auto_switch_allowed(false, true));
        assert!(!auto_switch_allowed(true, true));
    }

    #[test]
    fn saved_mirror_resolves_or_falls_back_to_auto() {
        let mirrors = api_mirrors("https://main.example");
        assert_eq!(mirrors[0].1, "https://main.example");
        // Пункт 0 — «Авто», дальше зеркала в порядке api_mirrors.
        assert_eq!(server_items(&mirrors)[0], AUTO_SERVER_LABEL);
        assert_eq!(server_items(&mirrors).len(), mirrors.len() + 1);
        // Нет сохранённого выбора → «Авто».
        assert_eq!(server_item_index(&mirrors, None), 0);
        // Зеркало, которого больше нет в сборке → «Авто», а не мёртвый адрес.
        assert_eq!(server_item_index(&mirrors, Some("https://gone.example")), 0);

        let mirrors = vec![
            ("Основной".to_string(), "https://main.example".to_string()),
            ("Зеркало".to_string(), "https://mirror.example".to_string()),
        ];
        assert_eq!(server_item_index(&mirrors, Some("https://main.example")), 1);
        assert_eq!(
            server_item_index(&mirrors, Some("https://mirror.example")),
            2
        );
        // start_url в main: индекс 0 («Авто») стартует с основного адреса.
        for (idx, want) in [(0usize, 0usize), (1, 0), (2, 1)] {
            assert_eq!(mirrors[idx.saturating_sub(1)].1, mirrors[want].1);
        }
    }

    // Регрессия «сессия не восстанавливается»: keyring без фич-бэкендов собирается в
    // заглушку, которая держит секрет ВНУТРИ объекта Entry — следующий Entry уже пуст.
    // save_token верил set_password, обнулял fallback в settings.json, и токен исчезал.
    #[test]
    fn keyring_without_backend_is_not_trusted() {
        keyring::set_default_credential_builder(keyring::mock::default_credential_builder());
        assert!(
            !keyring_persists("test-token"),
            "заглушку keyring нельзя считать рабочим хранилищем — иначе токен теряется"
        );
    }

    // Проверка окружения, а не кода: живо ли системное хранилище на этой машине.
    // Запуск: `cargo test -- --ignored keyring_backend`. Служба своя, боевой токен не трогаем.
    // Провал = хранилища нет (headless/нет агента) → лаунчер уйдёт на fallback в settings.json.
    #[test]
    #[ignore]
    fn keyring_backend_roundtrips_on_this_machine() {
        keyring::set_default_credential_builder(keyring::default::default_credential_builder());
        let entry = keyring::Entry::new("xyz.projectminecraft.launcher.selftest", "probe")
            .expect("создать запись хранилища");
        entry.set_password("probe-value").expect("записать секрет");
        let read = keyring::Entry::new("xyz.projectminecraft.launcher.selftest", "probe")
            .and_then(|e| e.get_password());
        let _ = entry.delete_credential();
        assert_eq!(read.as_deref().ok(), Some("probe-value"));
    }

    // Авто-выбор: самый быстрый ИЗ ДОСТУПНЫХ; недоступные (None) не выбираются никогда,
    // а если не ответил никто — переключать не на что.
    #[test]
    fn auto_server_picks_fastest_reachable() {
        assert_eq!(best_ping_index(&[Some(120), Some(40), None]), Some(1));
        assert_eq!(best_ping_index(&[None, Some(900)]), Some(1));
        assert_eq!(best_ping_index(&[None, None]), None);
        assert_eq!(best_ping_index(&[]), None);
        // Равные пинги — берём первый (основной адрес приоритетнее зеркала).
        assert_eq!(best_ping_index(&[Some(50), Some(50)]), Some(0));
        assert_eq!(
            ranked_ping_indices(&[Some(120), None, Some(40), Some(40)]),
            vec![2, 3, 0]
        );
    }

    #[test]
    fn jwt_expiry_is_restored_for_account_card() {
        let payload =
            base64::engine::general_purpose::URL_SAFE_NO_PAD.encode(br#"{"exp":951782400}"#);
        let token = format!("header.{payload}.signature");
        assert_eq!(token_expiry_label(&token), "2000-02-29 00:00:00");
        assert_eq!(token_expiry_label("broken"), "");
    }

    #[test]
    fn mirror_label_shows_ping_or_unavailable() {
        assert_eq!(mirror_label("Основной", Some(42)), "Основной — 42 мс");
        assert_eq!(mirror_label("Зеркало", None), "Зеркало — недоступен");
    }

    #[test]
    fn fast_failure_only_on_quick_nonzero_exit() {
        // Мгновенный краш: быстро + ошибка → распознаём.
        assert!(is_fast_launch_failure(Duration::from_secs(2), false));
        // Быстрый, но успешный выход (игрок сразу закрыл) — не ошибка.
        assert!(!is_fast_launch_failure(Duration::from_secs(2), true));
        // Долгая сессия с ненулевым кодом — нормальное закрытие, не мгновенный краш.
        assert!(!is_fast_launch_failure(Duration::from_secs(120), false));
    }

    #[test]
    fn launch_failure_message_has_code_path_and_tail() {
        let msg = launch_failure_message(
            Some(1),
            Path::new("/data/profile/launch.log"),
            "Error: Unable to access jarfile client.jar",
        );
        assert!(msg.contains("код 1"));
        assert!(msg.contains("/data/profile/launch.log"));
        assert!(msg.contains("Unable to access jarfile"));
    }

    #[test]
    fn launch_failure_message_handles_signal_and_empty_tail() {
        let msg = launch_failure_message(None, Path::new("/x/launch.log"), "");
        assert!(msg.contains("аварийно (сигнал)"));
        assert!(!msg.contains("Последние строки"));
    }

    #[test]
    fn read_log_tail_returns_last_lines() {
        let path = std::env::temp_dir().join("pjm_launch_tail_test.log");
        fs::write(&path, "l1\nl2\nl3\nl4\nl5\n").unwrap();
        let tail = read_log_tail(&path, 2);
        assert_eq!(tail, "l4\nl5");
        let _ = fs::remove_file(&path);
    }

    #[test]
    fn read_log_tail_missing_file_is_empty() {
        assert_eq!(read_log_tail(Path::new("/no/such/pjm_launch.log"), 10), "");
    }

    #[test]
    fn format_log_timestamp_known_values() {
        assert_eq!(format_log_timestamp(0), "1970-01-01 00:00:00");
        assert_eq!(format_log_timestamp(86_400), "1970-01-02 00:00:00");
        // Високосный год: 29 февраля 2000.
        assert_eq!(format_log_timestamp(951_782_400), "2000-02-29 00:00:00");
        assert_eq!(format_log_timestamp(1_751_993_669), "2025-07-08 16:54:29");
    }

    #[test]
    fn append_error_log_appends_timestamped_lines() {
        let dir = std::env::temp_dir().join("pjm_sync_log_append_test");
        let _ = fs::remove_dir_all(&dir);
        let path = dir.join("sync-errors.log");

        append_error_log(&path, 0, "первая ошибка").unwrap();
        append_error_log(&path, 86_400, "вторая ошибка").unwrap();

        let content = fs::read_to_string(&path).unwrap();
        let lines: Vec<&str> = content.lines().collect();
        assert_eq!(lines.len(), 2);
        assert_eq!(lines[0], "[1970-01-01 00:00:00 UTC] первая ошибка");
        assert_eq!(lines[1], "[1970-01-02 00:00:00 UTC] вторая ошибка");
        let _ = fs::remove_dir_all(&dir);
    }

    #[test]
    fn append_error_log_truncates_oversized_log() {
        let dir = std::env::temp_dir().join("pjm_sync_log_trunc_test");
        let _ = fs::remove_dir_all(&dir);
        let path = dir.join("sync-errors.log");
        fs::create_dir_all(&dir).unwrap();
        fs::write(&path, "x".repeat(ERROR_LOG_MAX_BYTES as usize + 1)).unwrap();

        append_error_log(&path, 0, "свежая ошибка").unwrap();

        let content = fs::read_to_string(&path).unwrap();
        // Разросшийся лог начат заново: старый мусор выброшен, новая запись на месте.
        assert_eq!(content, "[1970-01-01 00:00:00 UTC] свежая ошибка\n");
        let _ = fs::remove_dir_all(&dir);
    }

    #[test]
    fn discord_rpc_enabled_defaults_true_when_absent() {
        // Старый settings.json без поля discord_rpc_enabled должен десериализоваться
        // с дефолтом true (фича включена по умолчанию).
        let json = r#"{"memoryGb":4,"memoryAuto":true,"useDiscreteGpu":true}"#;
        let s: LauncherSettings = serde_json::from_str(json).unwrap();
        assert!(s.discord_rpc_enabled);
        assert!(s.install_root.is_none());
    }

    #[test]
    fn custom_install_root_contains_user_profiles() {
        let root = test_root("custom_install_root_contains_user_profiles");
        let user = AuthUser {
            id: "local-id".to_string(),
            login: "Player".to_string(),
            provider_uuid: "user-uuid".to_string(),
            is_slim: false,
            policy_accepted_version: 1,
        };

        let paths = profile_paths_at_root(&user, "profile-id", &root).unwrap();

        assert_eq!(
            paths.files_root,
            root.join("users/user-uuid/profiles/profile-id/files")
        );
        let _ = fs::remove_dir_all(root);
    }

    #[test]
    fn removes_module_path_entries_from_classpath() {
        let separator = classpath_separator();
        let module_path = [
            "libraries/cpw/mods/bootstraplauncher/2.0.2/bootstraplauncher-2.0.2.jar",
            "libraries/cpw/mods/securejarhandler/3.0.8/securejarhandler-3.0.8.jar",
        ]
        .join(&separator.to_string());
        let classpath = [
            "libraries/cpw/mods/bootstraplauncher/2.0.2/bootstraplauncher-2.0.2.jar",
            "libraries/com/google/code/gson/gson/2.10.1/gson-2.10.1.jar",
            "libraries/cpw/mods/securejarhandler/3.0.8/securejarhandler-3.0.8.jar",
        ]
        .join(&separator.to_string());
        let mut command = vec![
            "java".to_string(),
            "-p".to_string(),
            module_path,
            "-cp".to_string(),
            classpath,
            "cpw.mods.bootstraplauncher.BootstrapLauncher".to_string(),
        ];

        remove_module_path_entries_from_classpath(&mut command);

        let filtered = &command[4];
        assert!(!filtered.contains("bootstraplauncher"));
        assert!(!filtered.contains("securejarhandler"));
        assert!(filtered.contains("gson-2.10.1.jar"));
    }

    #[test]
    fn default_memory_is_auto_eight_gb() {
        let settings = LauncherSettings::default();

        assert!(settings.memory_auto);
        // Чистая версия с фиксированным максимумом: тест не зависит от ОЗУ хоста.
        assert_eq!(effective_memory_capped(&settings, 64), 8);
    }

    #[test]
    fn memory_is_capped_by_system_total() {
        // Авто-дефолт (8 ГБ) на машине с 6 ГБ ужимается до 6.
        let auto = LauncherSettings::default();
        assert_eq!(effective_memory_capped(&auto, 6), 6);

        // Ручное значение не может превышать ОЗУ машины.
        let manual = LauncherSettings {
            memory_auto: false,
            memory_gb: 64,
            ..Default::default()
        };
        assert_eq!(effective_memory_capped(&manual, 32), 32);
        assert_eq!(effective_memory_capped(&manual, 64), 64);
    }

    #[test]
    fn jvm_args_with_memory_replaces_existing_heap_args() {
        let args = jvm_args_with_memory("-Xmx4G -Xms2G -Dfoo=bar", 8).unwrap();

        assert_eq!(args, vec!["-Xmx8G", "-Dfoo=bar"]);
    }

    #[test]
    fn modified_managed_file_forces_download() {
        let root = test_root("modified_managed_file_forces_download");
        write_test_file(&root.join("mods/example.jar"), "changed");
        let expected = hex_hash(Sha256::digest(b"expected").as_slice());
        let file = test_manifest_file("mods/example.jar", &expected, "expected".len() as i64);

        assert!(needs_download(&root, &file).unwrap());

        let _ = fs::remove_dir_all(root);
    }

    #[test]
    fn startup_profile_check_trusts_unchanged_file_metadata() {
        let root = test_root("startup_profile_check_fast_path");
        let path = root.join("mods/example.jar");
        write_test_file(&path, "official");
        let metadata = fs::metadata(&path).unwrap();
        let local = LocalFileRecord {
            path: "mods/example.jar".to_string(),
            // Намеренно неверный hash: совпавшие сохранённые метаданные должны
            // пропустить дорогое чтение файла на стартовом экране.
            hash_sha256: "not-used-on-fast-path".to_string(),
            size: metadata.len() as i64,
            mtime_millis: file_mtime_millis(&metadata),
        };
        let file = test_manifest_file(
            "mods/example.jar",
            "not-used-on-fast-path",
            metadata.len() as i64,
        );

        assert!(!startup_file_needs_download(&root, &file, &local).unwrap());

        let _ = fs::remove_dir_all(root);
    }

    #[test]
    fn first_profile_event_connection_does_not_repeat_bootstrap_check() {
        let mut has_connected = false;

        assert!(!profile_event_connection_needs_catch_up(&mut has_connected));
        assert!(profile_event_connection_needs_catch_up(&mut has_connected));
    }

    #[test]
    fn profile_state_distinguishes_missing_ready_and_modified_files() {
        let profile_root = test_root("profile_install_state");
        let files_root = profile_root.join("files");
        let paths = ProfilePaths {
            profile_root: profile_root.clone(),
            files_root: files_root.clone(),
            manifest_path: profile_root.join("manifest.json"),
        };
        let hash = hex_hash(Sha256::digest(b"official").as_slice());
        let manifest = test_manifest(
            vec![test_manifest_file(
                "mods/official.jar",
                &hash,
                "official".len() as i64,
            )],
            Vec::new(),
        );

        assert_eq!(
            profile_install_state(&paths, &manifest).unwrap(),
            ProfileInstallState::Missing
        );
        write_test_file(&files_root.join("mods/official.jar"), "official");
        save_local_manifest(&paths.manifest_path, &files_root, &manifest).unwrap();
        assert_eq!(
            profile_install_state(&paths, &manifest).unwrap(),
            ProfileInstallState::Ready
        );
        write_test_file(&files_root.join("mods/official.jar"), "modified");
        assert_eq!(
            profile_install_state(&paths, &manifest).unwrap(),
            ProfileInstallState::UpdateAvailable
        );

        let _ = fs::remove_dir_all(profile_root);
    }

    #[test]
    fn profile_state_detects_new_manifest_version() {
        let profile_root = test_root("profile_manifest_version");
        let files_root = profile_root.join("files");
        let paths = ProfilePaths {
            profile_root: profile_root.clone(),
            files_root: files_root.clone(),
            manifest_path: profile_root.join("manifest.json"),
        };
        let hash = hex_hash(Sha256::digest(b"official").as_slice());
        let old = test_manifest(
            vec![test_manifest_file(
                "mods/official.jar",
                &hash,
                "official".len() as i64,
            )],
            Vec::new(),
        );
        write_test_file(&files_root.join("mods/official.jar"), "official");
        save_local_manifest(&paths.manifest_path, &files_root, &old).unwrap();
        let mut updated = old.clone();
        updated.profile.manifest_version += 1;

        assert_eq!(
            profile_install_state(&paths, &updated).unwrap(),
            ProfileInstallState::UpdateAvailable
        );

        let _ = fs::remove_dir_all(profile_root);
    }

    #[test]
    fn local_profile_watch_detects_changed_managed_file() {
        let profile_root = test_root("local_profile_watch");
        let install_root = profile_root.join("install");
        let user = AuthUser {
            id: "id".to_string(),
            login: "Player".to_string(),
            provider_uuid: "uuid".to_string(),
            is_slim: false,
            policy_accepted_version: 1,
        };
        let profile = ProfileSummary {
            id: "profile".to_string(),
            name: "Profile".to_string(),
            game_version: "1.21.1".to_string(),
            is_active: true,
        };
        let paths = profile_paths_at_root(&user, &profile.id, &install_root).unwrap();
        let hash = hex_hash(Sha256::digest(b"official").as_slice());
        let manifest = test_manifest(
            vec![test_manifest_file(
                "mods/official.jar",
                &hash,
                "official".len() as i64,
            )],
            Vec::new(),
        );
        write_test_file(&paths.files_root.join("mods/official.jar"), "official");
        save_local_manifest(&paths.manifest_path, &paths.files_root, &manifest).unwrap();

        let mut verified_mtimes = HashMap::new();
        assert_eq!(
            local_profile_files_changed_at(&paths, &mut verified_mtimes, false).unwrap(),
            None
        );
        thread::sleep(Duration::from_millis(2));
        write_test_file(&paths.files_root.join("mods/official.jar"), "modified");
        let changed_path = paths.files_root.join("mods/official.jar");
        let changed_mtime = file_mtime_millis(&fs::metadata(&changed_path).unwrap());
        verified_mtimes.insert(changed_path, changed_mtime);
        assert_eq!(
            local_profile_files_changed_at(&paths, &mut verified_mtimes, false).unwrap(),
            None
        );
        assert_eq!(
            local_profile_files_changed_at(&paths, &mut verified_mtimes, true).unwrap(),
            Some(ProfileInstallState::UpdateAvailable)
        );

        fs::remove_file(&paths.manifest_path).unwrap();
        assert_eq!(
            local_profile_files_changed_at(&paths, &mut verified_mtimes, false).unwrap(),
            Some(ProfileInstallState::Missing)
        );

        let _ = fs::remove_dir_all(profile_root);
    }

    #[test]
    fn strict_cleanup_removes_unknown_files_and_keeps_preserved_paths() {
        let profile_root =
            test_root("strict_cleanup_removes_unknown_files_and_keeps_preserved_paths");
        let files_root = profile_root.join("files");
        write_test_file(&files_root.join("mods/official.jar"), "official");
        write_test_file(&files_root.join("mods/custom.jar"), "custom");
        write_test_file(&files_root.join("saves/world/level.dat"), "save");
        write_test_file(&files_root.join("options.txt"), "options");

        let hash = hex_hash(Sha256::digest(b"official").as_slice());
        let manifest = test_manifest(
            vec![test_manifest_file(
                "mods/official.jar",
                &hash,
                "official".len() as i64,
            )],
            vec!["saves/".to_string(), "options.txt".to_string()],
        );
        let paths = ProfilePaths {
            profile_root: profile_root.clone(),
            files_root: files_root.clone(),
            manifest_path: profile_root.join("manifest.json"),
        };

        cleanup_unmanaged_files(&paths, &manifest, &HashSet::new()).unwrap();

        assert!(files_root.join("mods/official.jar").exists());
        assert!(!files_root.join("mods/custom.jar").exists());
        assert!(files_root.join("saves/world/level.dat").exists());
        assert!(files_root.join("options.txt").exists());

        let _ = fs::remove_dir_all(profile_root);
    }

    #[cfg(unix)]
    #[test]
    fn symlink_managed_file_forces_download_and_whitelist_symlink_is_kept() {
        let profile_root =
            test_root("symlink_managed_file_forces_download_and_whitelist_symlink_is_kept");
        let files_root = profile_root.join("files");
        fs::create_dir_all(files_root.join("mods")).unwrap();
        fs::create_dir_all(files_root.join("saves")).unwrap();
        std::os::unix::fs::symlink("/tmp/managed-target", files_root.join("mods/official.jar"))
            .unwrap();
        std::os::unix::fs::symlink("/tmp/save-target", files_root.join("saves/link")).unwrap();

        let hash = hex_hash(Sha256::digest(b"official").as_slice());
        let file = test_manifest_file("mods/official.jar", &hash, "official".len() as i64);
        let manifest = test_manifest(vec![file.clone()], vec!["saves/".to_string()]);
        let paths = ProfilePaths {
            profile_root: profile_root.clone(),
            files_root: files_root.clone(),
            manifest_path: profile_root.join("manifest.json"),
        };

        assert!(needs_download(&files_root, &file).unwrap());
        cleanup_unmanaged_files(&paths, &manifest, &HashSet::new()).unwrap();

        assert!(fs::symlink_metadata(files_root.join("saves/link"))
            .unwrap()
            .file_type()
            .is_symlink());

        let _ = fs::remove_dir_all(profile_root);
    }

    #[test]
    fn safe_join_allows_nested_path_with_missing_parents() {
        // Регресс на Windows-баг: вложенный файл, чьи родительские папки ещё не
        // созданы, должен резолвиться, а не ронять синк «Путь выходит за папку».
        let root = test_root("safe_join_allows_nested_path_with_missing_parents");

        let joined = safe_join(&root, "mods/sub/example.jar").unwrap();
        assert!(joined.starts_with(&root));
        assert!(joined.ends_with("mods/sub/example.jar"));

        let _ = fs::remove_dir_all(root);
    }

    #[test]
    fn safe_join_rejects_traversal() {
        let root = test_root("safe_join_rejects_traversal");

        assert!(safe_join(&root, "../escape.jar").is_err());
        assert!(safe_join(&root, "mods/../../escape.jar").is_err());

        let _ = fs::remove_dir_all(root);
    }

    fn test_root(name: &str) -> PathBuf {
        let nanos = std::time::SystemTime::now()
            .duration_since(UNIX_EPOCH)
            .unwrap()
            .as_nanos();
        let root = std::env::temp_dir().join(format!(
            "launcher-slint-test-{}-{}-{}",
            std::process::id(),
            name,
            nanos
        ));
        let _ = fs::remove_dir_all(&root);
        fs::create_dir_all(&root).unwrap();
        root
    }

    fn write_test_file(path: &Path, data: &str) {
        fs::create_dir_all(path.parent().unwrap()).unwrap();
        fs::write(path, data).unwrap();
    }

    fn test_manifest(files: Vec<ManifestFile>, preserve_paths: Vec<String>) -> Manifest {
        Manifest {
            profile: ManifestProfile {
                id: "profile-id".to_string(),
                name: "Profile".to_string(),
                java_version: 21,
                jvm_args: String::new(),
                java_path_windows: String::new(),
                java_path_linux: String::new(),
                java_path_macos: String::new(),
                launch_command_windows: String::new(),
                launch_command_linux: String::new(),
                launch_command_macos: String::new(),
                manifest_version: 1,
            },
            file_count: files.len(),
            total_size: files.iter().map(|file| file.size).sum(),
            files,
            preserve_paths,
            bundle: None,
        }
    }

    fn test_manifest_file(path: &str, hash: &str, size: i64) -> ManifestFile {
        ManifestFile {
            id: path.to_string(),
            name: Path::new(path)
                .file_name()
                .unwrap()
                .to_string_lossy()
                .to_string(),
            path: path.to_string(),
            download_url: format!("/{}", path),
            hash_sha256: hash.to_string(),
            size,
            file_type: "test".to_string(),
            executable: false,
        }
    }
}
