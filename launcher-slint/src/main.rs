use std::collections::{HashMap, HashSet};
use std::fs::{self, File};
use std::io::{BufRead, BufReader, Read, Write};
use std::path::{Component, Path, PathBuf};
use std::process::{Command, ExitStatus};
use std::sync::atomic::{AtomicU64, AtomicUsize, Ordering};
use std::sync::{Arc, Mutex};
use std::thread;
use std::time::{Duration, UNIX_EPOCH};

use directories::ProjectDirs;
use rayon::prelude::*;
use reqwest::blocking::Client;
use serde::{Deserialize, Serialize};
use sha1::Sha1;
use sha2::{Digest, Sha256};
use slint::{ComponentHandle, ModelRc, SharedString, VecModel, Weak};

mod anticheat;

slint::include_modules!();

const KEYRING_SERVICE: &str = "xyz.projectminecraft.launcher";
const KEYRING_USER: &str = "launcher-auth-token";
const JAVA_RUNTIME_INDEX_URL: &str =
    "https://piston-meta.mojang.com/v1/products/java-runtime/2ec0cc96c44e5a76b9c8b7c39df7210883d12871/all.json";
// Маркер в тексте ошибки запуска: означает, что игру закрыл античит (kick).
// Play-хендлер показывает по нему полноэкранное уведомление вместо обычной ошибки.
const ANTICHEAT_KICK_PREFIX: &str = "\u{1}ANTICHEAT_KICK\u{1}";
const DEFAULT_MEMORY_GB: i32 = 8;
const MIN_MEMORY_GB: i32 = 2;
const MAX_MEMORY_GB: i32 = 64;

#[derive(Clone)]
struct AppConfig {
    api_url: String,
}

#[derive(Clone, Default)]
struct RuntimeState {
    token: String,
    user: Option<AuthUser>,
    profiles: Vec<ProfileSummary>,
    selected_profile_id: Option<String>,
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
}

impl Default for LauncherSettings {
    fn default() -> Self {
        Self {
            auth_token: None,
            last_user_uuid: None,
            selected_profiles: HashMap::new(),
            memory_gb: DEFAULT_MEMORY_GB,
            memory_auto: true,
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
}

struct ProfilePaths {
    profile_root: PathBuf,
    files_root: PathBuf,
    manifest_path: PathBuf,
}

fn main() -> Result<(), slint::PlatformError> {
    std::env::set_var("SLINT_SCALE_FACTOR", "0.95");

    let default_api_url = option_env!("LAUNCHER_DEFAULT_API_URL")
        .unwrap_or("http://127.0.0.1:8080")
        .to_string();
    let config = AppConfig {
        api_url: std::env::var("LAUNCHER_API_URL").unwrap_or(default_api_url),
    };

    let app = AppWindow::new()?;
    app.window().set_size(slint::LogicalSize::new(1152.0, 720.0));
    app.set_api_url(config.api_url.clone().into());
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
    apply_launcher_settings(&app, &load_settings().unwrap_or_default());
    apply_install_folder_label(&app, &state);

    register_login_handler(&app, config.clone(), state.clone(), session_generation.clone());
    register_logout_handler(&app, state.clone(), session_generation.clone());
    register_settings_handler(&app, state.clone());
    register_play_handler(&app, config.clone(), state.clone());
    restore_saved_session(&app, config, state, session_generation);

    app.window().on_close_requested(|| {
        let _ = slint::quit_event_loop();
        slint::CloseRequestResponse::HideWindow
    });

    app.show()?;
    slint::run_event_loop_until_quit()
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

        let app_weak = login_app.clone();
        let config = config.clone();
        let state = state.clone();
        let generation = generation.clone();
        thread::spawn(move || {
            let result = login_and_bootstrap(&config, login, password, totp);
            let _ = slint::invoke_from_event_loop(move || {
                if let Some(app) = app_weak.upgrade() {
                    app.set_is_loading(false);
                    match result {
                        Ok(session) => apply_session(&app, &state, &config, &generation, session),
                        Err(error) => {
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

fn register_logout_handler(
    app: &AppWindow,
    state: Arc<Mutex<RuntimeState>>,
    generation: Arc<AtomicU64>,
) {
    let logout_app = app.as_weak();
    app.on_logout_requested(move || {
        let _ = delete_token();
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
            apply_install_folder_label(&app, &state);
            app.set_message("Сессия завершена.".into());
        }
    });
}

fn register_settings_handler(app: &AppWindow, state: Arc<Mutex<RuntimeState>>) {
    let settings_app = app.as_weak();
    let settings_state = state.clone();
    app.on_settings_requested(move || {
        if let Some(app) = settings_app.upgrade() {
            apply_launcher_settings(&app, &load_settings().unwrap_or_default());
            apply_install_folder_label(&app, &settings_state);
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

    let folder_app = app.as_weak();
    app.on_open_install_folder_requested(move || {
        if let Some(app) = folder_app.upgrade() {
            let folder = state
                .lock()
                .map_err(|_| "Не удалось прочитать состояние лаунчера.".to_string())
                .and_then(|state| install_folder_for_state(&state));

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

    app.on_open_url(|url| {
        let url = url.to_string();
        let mut command = if cfg!(target_os = "windows") {
            let mut cmd = Command::new("cmd");
            cmd.args(["/C", "start", &url]);
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

fn register_play_handler(app: &AppWindow, config: AppConfig, state: Arc<Mutex<RuntimeState>>) {
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

        if let Some(app) = play_app.upgrade() {
            app.set_is_syncing(true);
            app.set_settings_visible(false);
            app.set_download_panel_visible(true);
            app.set_download_phase("Получаем профиль".into());
            app.set_download_file(profile.name.clone().into());
            app.set_download_counter("0%".into());
            app.set_download_progress(0.0);
            app.set_message("Готовим профиль к запуску...".into());
        }

        let app_weak = play_app.clone();
        let config = config.clone();
        thread::spawn(move || {
            let result = sync_and_launch(&config, &token, &user, &profile, &app_weak);
            let _ = slint::invoke_from_event_loop(move || {
                if let Some(app) = app_weak.upgrade() {
                    let _ = app.show();
                    app.set_is_syncing(false);
                    match result {
                        Ok(message) => {
                            app.set_download_phase("Готово".into());
                            app.set_download_file("Minecraft закрыт".into());
                            app.set_download_counter("100%".into());
                            app.set_download_progress(1.0);
                            app.set_download_panel_visible(false);
                            app.set_message(message.into());
                        }
                        Err(message) => {
                            if let Some(alert) = message.strip_prefix(ANTICHEAT_KICK_PREFIX) {
                                // Игру закрыл античит — полноэкранное уведомление.
                                app.set_download_panel_visible(false);
                                app.set_message(SharedString::default());
                                app.set_anticheat_alert(alert.into());
                            } else {
                                app.set_download_phase("Ошибка".into());
                                app.set_download_file(message.clone().into());
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

fn restore_saved_session(
    app: &AppWindow,
    config: AppConfig,
    state: Arc<Mutex<RuntimeState>>,
    generation: Arc<AtomicU64>,
) {
    let token = match read_token() {
        Ok(token) if !token.trim().is_empty() => token,
        _ => return,
    };

    app.set_is_loading(true);
    app.set_message("Восстанавливаем сессию...".into());

    let app_weak = app.as_weak();
    thread::spawn(move || {
        let result = restore_session(&config, token);
        let _ = slint::invoke_from_event_loop(move || {
            if let Some(app) = app_weak.upgrade() {
                app.set_is_loading(false);
                match result {
                    Ok(session) => apply_session(&app, &state, &config, &generation, session),
                    Err(message) => {
                        if should_forget_saved_session(&message) {
                            let _ = delete_token();
                        }
                        app.set_message(message.into());
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
    bootstrap_session(
        config,
        token,
        user,
        String::default(),
        "Сессия восстановлена.".to_string(),
    )
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
    Ok(SessionData {
        token,
        user,
        expires_at,
        message,
        profiles,
        selected_profile_id,
        news,
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
    app.set_user_login(session.user.login.into());
    app.set_user_uuid(session.user.provider_uuid.into());
    app.set_is_slim(session.user.is_slim);
    app.set_token_expires_at(session.expires_at.into());
    app.set_password_value(SharedString::default());
    app.set_totp_value(SharedString::default());
    app.set_message(session.message.into());

    set_profile_ui(app, selected.as_ref());

    let news_model: Vec<NewsItem> = session
        .news
        .iter()
        .map(|item| NewsItem {
            title: item.title.clone().into(),
            date: format_news_date(&item.created_at).into(),
            body: item.body.clone().into(),
        })
        .collect();
    app.set_news_items(ModelRc::new(VecModel::from(news_model)));

    apply_install_folder_label(app, state);

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
}

/// Обновляет в UI поля выбранного профиля (или показывает «Нет профилей»).
fn set_profile_ui(app: &AppWindow, selected: Option<&ProfileSummary>) {
    if let Some(profile) = selected {
        app.set_has_profile(true);
        app.set_profile_status("Доступен".into());
        app.set_selected_profile_name(profile.name.clone().into());
        app.set_selected_profile_version(profile.game_version.clone().into());
    } else {
        app.set_has_profile(false);
        app.set_profile_status("Нет профилей".into());
        app.set_selected_profile_name(SharedString::default());
        app.set_selected_profile_version("-".into());
    }
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
        while generation.load(Ordering::SeqCst) == my_generation {
            let token = match state.lock() {
                Ok(state) => state.token.clone(),
                Err(_) => return,
            };
            if token.trim().is_empty() {
                return;
            }

            match stream_profile_events(&config, &token, &state, &app_weak, &generation, my_generation) {
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
) -> StreamOutcome {
    let client = match sse_client() {
        Ok(client) => client,
        Err(_) => return StreamOutcome::Disconnected,
    };
    let url = format!(
        "{}/api/profiles/events",
        config.api_url.trim_end_matches('/')
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
        if line.starts_with("data:") {
            refresh_profiles_now(config, state, app_weak);
        }
    }
    StreamOutcome::Disconnected
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
    let selected_id = user_uuid
        .and_then(|uuid| choose_profile_for_user(&uuid, &profiles).ok().flatten());

    if let Ok(mut state) = state.lock() {
        state.profiles = profiles.clone();
        state.selected_profile_id = selected_id.clone();
    }

    let selected = selected_id
        .as_ref()
        .and_then(|id| profiles.iter().find(|profile| &profile.id == id))
        .cloned();
    let app_weak = app_weak.clone();
    let _ = slint::invoke_from_event_loop(move || {
        if let Some(app) = app_weak.upgrade() {
            set_profile_ui(&app, selected.as_ref());
        }
    });
}

/// HTTP-клиент для долгоживущего SSE-потока: без общего таймаута запроса,
/// но с TCP keepalive для обнаружения «мёртвых» соединений.
fn sse_client() -> Result<Client, String> {
    Client::builder()
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

    let url = format!("{}/api/auth/login", config.api_url.trim_end_matches('/'));
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
        .get(format!("{}/api/auth/me", config.api_url.trim_end_matches('/')))
        .bearer_auth(token)
        .send()
        .map_err(|_| "Backend лаунчера недоступен.".to_string())?;
    parse_json_response(response, "Не удалось восстановить пользователя")
}

fn fetch_profiles(config: &AppConfig, token: &str) -> Result<Vec<ProfileSummary>, String> {
    let client = http_client()?;
    let response = client
        .get(format!("{}/api/profiles", config.api_url.trim_end_matches('/')))
        .bearer_auth(token)
        .send()
        .map_err(|_| "Не удалось получить профили проекта.".to_string())?;
    parse_json_response(response, "Backend вернул некорректный список профилей")
}

fn fetch_news(config: &AppConfig, token: &str) -> Vec<NewsSummary> {
    let client = match http_client() {
        Ok(client) => client,
        Err(_) => return Vec::new(),
    };
    let url = format!("{}/api/news?limit=20", config.api_url.trim_end_matches('/'));
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
        config.api_url.trim_end_matches('/')
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

// Гарантирует наличие authlib-injector.jar в служебной папке лаунчера (вне
// files/, чтобы cleanup его не удалял). Качает с бэкенда при отсутствии.
fn ensure_authlib_injector(config: &AppConfig) -> Option<PathBuf> {
    let dir = project_dirs().ok()?.data_dir().to_path_buf();
    let path = dir.join("authlib-injector.jar");
    if path.exists() {
        return Some(path);
    }
    let client = download_client().ok()?;
    let url = format!(
        "{}/api/yggdrasil/authlib-injector.jar",
        config.api_url.trim_end_matches('/')
    );
    let response = client.get(url).send().ok()?;
    if !response.status().is_success() {
        return None;
    }
    let bytes = response.bytes().ok()?;
    fs::create_dir_all(&dir).ok()?;
    let tmp = path.with_extension("jar.part");
    fs::write(&tmp, &bytes).ok()?;
    fs::rename(&tmp, &path).ok()?;
    Some(path)
}

// Гарантирует наличие agent.jar античита в служебной папке. Всегда пытается
// скачать свежую версию с бэкенда (jar маленький), при ошибке использует кэш.
fn ensure_agent_jar(config: &AppConfig) -> Option<PathBuf> {
    let dir = project_dirs().ok()?.data_dir().to_path_buf();
    let path = dir.join("anticheat-agent.jar");
    let url = format!(
        "{}/api/anticheat/agent.jar",
        config.api_url.trim_end_matches('/')
    );
    if let Ok(client) = download_client() {
        if let Ok(response) = client.get(&url).send() {
            if response.status().is_success() {
                if let Ok(bytes) = response.bytes() {
                    if fs::create_dir_all(&dir).is_ok() {
                        let tmp = path.with_extension("jar.part");
                        if fs::write(&tmp, &bytes).is_ok() && fs::rename(&tmp, &path).is_ok() {
                            return Some(path);
                        }
                    }
                }
            }
        }
    }
    // Сеть недоступна — используем ранее скачанный jar, если он есть.
    if path.exists() {
        Some(path)
    } else {
        None
    }
}

// Имя нативной JVMTI-библиотеки и токен ОС для эндпоинта раздачи. None — ОС без
// собранной нативной части (агент тогда не инжектится, его отсутствие зафиксирует
// Java-агент как детект).
fn native_agent_target() -> Option<(&'static str, &'static str)> {
    if cfg!(target_os = "linux") {
        Some(("linux", "libanticheat.so"))
    } else if cfg!(target_os = "windows") {
        Some(("windows", "anticheat.dll"))
    } else {
        None
    }
}

// Гарантирует наличие нативной библиотеки античита в служебной папке (качает с
// бэкенда по текущей ОС). Возвращает путь к ней.
fn ensure_native_agent(config: &AppConfig) -> Option<PathBuf> {
    let (os_token, file_name) = native_agent_target()?;
    let dir = project_dirs().ok()?.data_dir().to_path_buf();
    let path = dir.join(file_name);
    let url = format!(
        "{}/api/anticheat/native/{}",
        config.api_url.trim_end_matches('/'),
        os_token
    );
    if let Ok(client) = download_client() {
        if let Ok(response) = client.get(&url).send() {
            if response.status().is_success() {
                if let Ok(bytes) = response.bytes() {
                    if fs::create_dir_all(&dir).is_ok() {
                        let tmp = path.with_extension("part");
                        if fs::write(&tmp, &bytes).is_ok() && fs::rename(&tmp, &path).is_ok() {
                            return Some(path);
                        }
                    }
                }
            }
        }
    }
    if path.exists() {
        Some(path)
    } else {
        None
    }
}

// После закрытия игры гасим accessToken, чтобы скопированную команду запуска
// нельзя было переиспользовать позже. Best-effort: ошибки игнорируем.
fn invalidate_yggdrasil_session(config: &AppConfig, access_token: &str) {
    let Ok(client) = http_client() else {
        return;
    };
    let url = format!(
        "{}/api/yggdrasil/authserver/invalidate",
        config.api_url.trim_end_matches('/')
    );
    let _ = client
        .post(url)
        .json(&serde_json::json!({ "accessToken": access_token }))
        .send();
}

fn fetch_manifest(config: &AppConfig, token: &str, profile_id: &str) -> Result<Manifest, String> {
    let client = http_client()?;
    let response = client
        .get(format!(
            "{}/api/profiles/{}/manifest",
            config.api_url.trim_end_matches('/'),
            profile_id
        ))
        .bearer_auth(token)
        .send()
        .map_err(|_| "Не удалось получить manifest профиля.".to_string())?;
    parse_json_response(response, "Backend вернул некорректный manifest")
}

fn parse_json_response<T: for<'de> Deserialize<'de>>(
    response: reqwest::blocking::Response,
    fallback: &str,
) -> Result<T, String> {
    let status = response.status();
    if status.is_success() {
        return response.json::<T>().map_err(|_| fallback.to_string());
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
    let manifest = fetch_manifest(config, token, &profile.id)?;
    let paths = profile_paths(user, &manifest.profile.id)?;
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
    download_files(config, token, app, &paths.files_root, &manifest, &files_to_download)?;

    post_progress(app, "Проверяем Java", "Runtime текущей ОС", "92%", 0.92, true);
    let java_managed_paths = ensure_java_runtime(app, &paths, &manifest)?;

    post_progress(app, "Очищаем", "Удаляем устаревшие файлы", "96%", 0.96, true);
    cleanup_unmanaged_files(&paths, &manifest, &java_managed_paths)?;
    save_local_manifest(&paths.manifest_path, &paths.files_root, &manifest)?;

    post_progress(app, "Запускаем", &manifest.profile.name, "99%", 0.99, true);
    launch_profile(app, config, &paths, &manifest, token)
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
        post_progress(app, "Скачиваем", "Все файлы уже актуальны", "92%", 0.92, true);
        return Ok(());
    }

    let client = download_client()?;
    let total_bytes = files.iter().map(|file| file.size.max(0) as u64).sum::<u64>().max(1);
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
                let file_bytes = download_one_file(&client, config, token, files_root, file)?;

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
        &format!("{} файлов, {}", manifest.file_count, format_bytes(manifest.total_size)),
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
    client: &Client,
    config: &AppConfig,
    token: &str,
    files_root: &Path,
    file: &ManifestFile,
) -> Result<u64, String> {
    let target = safe_join(files_root, &file.path)?;
    if let Some(parent) = target.parent() {
        fs::create_dir_all(parent).map_err(|_| "Не удалось создать папку для файла.".to_string())?;
    }

    let url = absolute_api_url(config, &file.download_url);
    let mut response = client
        .get(url)
        .bearer_auth(token)
        .send()
        .map_err(|_| format!("Не удалось скачать {}", file.path))?;
    if !response.status().is_success() {
        return Err(format!("Ошибка скачивания {}: HTTP {}", file.path, response.status().as_u16()));
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
    output.flush().map_err(|_| format!("Ошибка записи {}", file.path))?;

    let hash = hex_hash(hasher.finalize().as_slice());
    if hash != file.hash_sha256.to_lowercase() {
        let _ = fs::remove_file(&temp_path);
        return Err(format!("Hash mismatch: {}", file.path));
    }
    if file.size >= 0 && file_bytes != file.size as u64 {
        let _ = fs::remove_file(&temp_path);
        return Err(format!("Размер файла изменился: {}", file.path));
    }

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
        ensure_directory(&target, &format!("Не удалось создать папку Java runtime: {}", path))?;
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
        .map(|&(path, entry, download)| -> Result<Option<JavaRuntimeDownloadTask>, String> {
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
        })
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
        post_progress(app, "Проверяем Java", "Java runtime актуален", "94%", 0.94, true);
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
    if metadata.file_type().is_symlink() || !metadata.is_file() || metadata.len() != download.size as u64 {
        return Ok(true);
    }
    let hash = hash_file_sha1(&target)?;
    Ok(hash != download.sha1.to_lowercase())
}

fn needs_download(
    files_root: &Path,
    file: &ManifestFile,
) -> Result<bool, String> {
    let target = safe_join(files_root, &file.path)?;
    let metadata = match fs::symlink_metadata(&target) {
        Ok(metadata) => metadata,
        Err(error) if error.kind() == std::io::ErrorKind::NotFound => return Ok(true),
        Err(_) => return Err(format!("Не удалось проверить {}", file.path)),
    };
    if metadata.file_type().is_symlink() || !metadata.is_file() || metadata.len() != file.size as u64 {
        return Ok(true);
    }

    let expected = file.hash_sha256.to_lowercase();
    let hash = hash_file(&target)?;
    Ok(hash != expected)
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
    cleanup_directory(&paths.files_root, &paths.files_root, &allowed_paths, &preserve_paths)
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

        let metadata = fs::symlink_metadata(&path)
            .map_err(|_| format!("Не удалось проверить {}", rel))?;
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
) -> Result<String, String> {
    // Pre-launch античит: сбор HWID, скан процессов, проверка банов на бэкенде.
    // Блокирует запуск (Err) при бане HWID/аккаунта до получения игровой сессии.
    let guard = anticheat::pre_launch_guard(config, token)?;

    // Игровая сессия привязывается к nonce из handshake/init. confirm выполняет
    // Java-агент внутри JVM (M3) — без него сессия не Verified и сервер отклонит join.
    let session = fetch_yggdrasil_session(config, token, &guard.nonce)?;

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
    let mut jvm_args = jvm_args_with_memory(&manifest.profile.jvm_args, effective_memory_gb(&settings))?;

    // Подключаем authlib-injector как javaagent, указывая на наш Yggdrasil-сервер.
    // Jar — launcher-managed (качается с бэкенда), лежит вне files/, поэтому
    // cleanup его не трогает. Клиент и игровой сервер должны указывать на один
    // и тот же базовый URL (GML-совместимый путь).
    if let Some(injector) = ensure_authlib_injector(config) {
        let ygg_url = format!(
            "{}/api/v1/integrations/authlib/minecraft",
            config.api_url.trim_end_matches('/')
        );
        jvm_args.insert(
            0,
            format!("-javaagent:{}={}", injector.to_string_lossy(), ygg_url),
        );
    }

    // Путь к kick-файлу: Java-агент пишет сюда причину перед убийством JVM, лаунчер
    // читает его после выхода игры, чтобы показать уведомление о попытке инжекта.
    let mut kick_file: Option<PathBuf> = None;

    // Инжект агентов античита. Только если handshake/init выдал токен — иначе агенты
    // бессильны, а сессия не пройдёт verified-гейт на join.
    if !guard.launch_token.is_empty() {
        // Нативный JVMTI-агент (M4): anti-inject/anti-debug + flag-файл для Java-агента.
        // Также запрещаем поздний attach к JVM (anti late-injection).
        if let Some(native) = ensure_native_agent(config) {
            let flag = native.with_file_name("ac_native.flag");
            let _ = fs::remove_file(&flag); // свежий старт: убираем прошлый флаг
            // КРИТИЧНО: чистим и файл событий, иначе Java-поллер при новом запуске
            // перечитает старые детекты прошлой (читерской) сессии и кикнет чистую игру.
            let _ = fs::remove_file(native.with_file_name("ac_native.flag.events"));
            jvm_args.insert(0, "-XX:+DisableAttachMechanism".to_string());
            jvm_args.insert(1, format!("-Dac.native.flag={}", flag.to_string_lossy()));
            jvm_args.insert(
                2,
                format!("-agentpath:{}={}", native.to_string_lossy(), flag.to_string_lossy()),
            );
        }

        // Java-агент (M3): confirm + рантайм-скан классов/модов + heartbeat.
        if let Some(agent) = ensure_agent_jar(config) {
            let kick = agent.with_file_name("ac_kick.flag");
            let _ = fs::remove_file(&kick); // свежий старт
            kick_file = Some(kick.clone());
            jvm_args.insert(0, format!("-Dac.token={}", guard.launch_token));
            jvm_args.insert(1, format!("-Dac.url={}", config.api_url.trim_end_matches('/')));
            jvm_args.insert(2, format!("-Dac.kickfile={}", kick.to_string_lossy()));
            jvm_args.insert(3, format!("-javaagent:{}", agent.to_string_lossy()));
        }
    }

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
    let mut child = process
        .spawn()
        .map_err(|err| format!("Не удалось запустить Minecraft: {}", err))?;

    post_game_started(app);
    let status = child
        .wait()
        .map_err(|err| format!("Не удалось дождаться закрытия Minecraft: {}", err))?;
    invalidate_yggdrasil_session(config, &session.access_token);

    // Если античит убил игру (kick-файл создан агентом) — возвращаем уведомление о
    // попытке инжекта вместо обычного сообщения о закрытии.
    if let Some(kick) = kick_file {
        if let Ok(content) = fs::read_to_string(&kick) {
            let _ = fs::remove_file(&kick);
            let reason = content
                .lines()
                .find_map(|l| l.strip_prefix("reason="))
                .unwrap_or("")
                .trim();
            return Err(format!("{}{}", ANTICHEAT_KICK_PREFIX, anticheat_kick_message(reason)));
        }
    }

    Ok(minecraft_exit_message(status))
}

// Текст уведомления игроку при кике античитом.
fn anticheat_kick_message(reason: &str) -> String {
    let detail = match reason {
        "illegal-class-name" => "обнаружена инъекция стороннего кода (чит-клиент)",
        "inject" => "обнаружена инъекция стороннего кода",
        "" => "обнаружена попытка вмешательства",
        other => other,
    };
    format!(
        "⛔ Игра закрыта системой защиты: {}. Уберите сторонние программы и запустите снова.",
        detail
    )
}

fn post_game_started(app: &Weak<AppWindow>) {
    let app = app.clone();
    let _ = slint::invoke_from_event_loop(move || {
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
    result.push(format!("-Xmx{}G", clamp_memory_gb(memory_gb)));
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
                    command[index + 1] = filter_classpath(&command[index + 1], separator, &module_entries);
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
    let mut normalized = entry.trim().trim_matches('"').trim_matches('\'').replace('\\', "/");
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
    let mut settings = load_settings().unwrap_or_default();
    settings.last_user_uuid = Some(user_uuid.to_string());

    let selected = settings
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
    save_settings(&settings)?;
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

fn apply_launcher_settings(app: &AppWindow, settings: &LauncherSettings) {
    let memory_gb = effective_memory_gb(settings);
    app.set_memory_gb(memory_gb);
    app.set_memory_auto(settings.memory_auto);
    app.set_memory_label(memory_label(settings).into());
}

fn apply_install_folder_label(app: &AppWindow, state: &Arc<Mutex<RuntimeState>>) {
    let folder = state
        .lock()
        .map_err(|_| "Не удалось прочитать состояние лаунчера.".to_string())
        .and_then(|state| install_folder_for_state(&state))
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
        let mut settings = load_settings().unwrap_or_default();
        update(&mut settings);
        settings.memory_gb = clamp_memory_gb(settings.memory_gb);

        match save_settings(&settings) {
            Ok(()) => {
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
    let memory_gb = if settings.memory_auto {
        DEFAULT_MEMORY_GB
    } else {
        settings.memory_gb
    };
    clamp_memory_gb(memory_gb)
}

fn clamp_memory_gb(value: i32) -> i32 {
    value.clamp(MIN_MEMORY_GB, MAX_MEMORY_GB)
}

fn default_memory_gb() -> i32 {
    DEFAULT_MEMORY_GB
}

fn default_memory_auto() -> bool {
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
    let _ = slint::invoke_from_event_loop(move || {
        if let Some(app) = app.upgrade() {
            app.set_download_phase(phase.into());
            app.set_download_file(file_name.into());
            app.set_download_counter(counter.into());
            app.set_download_progress(progress);
            app.set_download_panel_visible(visible);
        }
    });
}

fn http_client() -> Result<Client, String> {
    Client::builder()
        .timeout(Duration::from_secs(30))
        .build()
        .map_err(|_| "Не удалось создать HTTP клиент.".to_string())
}

/// Клиент для скачивания файлов: без общего таймаута (большие файлы качаются
/// дольше 30 с), мёртвое соединение обнаруживается connect-таймаутом и TCP keepalive.
fn download_client() -> Result<Client, String> {
    Client::builder()
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

fn save_token(token: &str) -> Result<(), String> {
    let keyring_result = keyring::Entry::new(KEYRING_SERVICE, KEYRING_USER)
        .map_err(|_| "Не удалось открыть системное хранилище токенов.".to_string())
        .and_then(|entry| {
            entry
                .set_password(token)
                .map_err(|_| "Не удалось сохранить токен авторизации.".to_string())
        });

    let mut settings = load_settings().unwrap_or_default();
    settings.auth_token = Some(token.to_string());
    let settings_result = save_settings(&settings);

    if keyring_result.is_ok() || settings_result.is_ok() {
        return Ok(());
    }
    Err(settings_result
        .err()
        .or_else(|| keyring_result.err())
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

    let mut settings = load_settings().unwrap_or_default();
    settings.auth_token = None;
    save_settings(&settings)
}

fn should_forget_saved_session(message: &str) -> bool {
    let normalized = message.to_lowercase();
    normalized.contains("http 401")
        || normalized.contains("требуется авторизация")
        || normalized.contains("сессия недействительна")
        || normalized.contains("unauthorized")
}

fn load_settings() -> Result<LauncherSettings, String> {
    let path = settings_path()?;
    if !path.exists() {
        return Ok(LauncherSettings::default());
    }
    let data = fs::read_to_string(path).map_err(|_| "Не удалось прочитать settings.json.".to_string())?;
    serde_json::from_str(&data).map_err(|_| "settings.json повреждён.".to_string())
}

fn save_settings(settings: &LauncherSettings) -> Result<(), String> {
    let path = settings_path()?;
    if let Some(parent) = path.parent() {
        fs::create_dir_all(parent).map_err(|_| "Не удалось создать папку настроек.".to_string())?;
    }
    let data = serde_json::to_string_pretty(settings)
        .map_err(|_| "Не удалось сохранить настройки.".to_string())?;
    fs::write(path, data).map_err(|_| "Не удалось записать settings.json.".to_string())
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
    let user_key = safe_component(if user.provider_uuid.is_empty() {
        &user.id
    } else {
        &user.provider_uuid
    });
    let profile_key = safe_component(profile_id);
    let profile_root = project_dirs()?
        .data_dir()
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
    let parent = path.parent().unwrap_or(root);
    let parent_abs = parent
        .canonicalize()
        .unwrap_or_else(|_| parent.to_path_buf());
    if parent_abs != root_abs && !parent_abs.starts_with(&root_abs) {
        return Err(format!("Путь выходит за папку профиля: {}", rel));
    }
    Ok(path)
}

fn hash_file(path: &Path) -> Result<String, String> {
    let mut file = File::open(path).map_err(|_| "Не удалось открыть файл для проверки.".to_string())?;
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

fn ensure_executable(path: &Path, executable: bool) -> Result<(), String> {
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
    matches!(root, "mods" | "libraries" | "versions" | "assets" | "runtime")
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
            config.api_url.trim_end_matches('/'),
            value.trim_start_matches('/')
        )
    }
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
        assert_eq!(effective_memory_gb(&settings), 8);
        assert_eq!(memory_label(&settings), "Авто · 8 ГБ");
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
    fn strict_cleanup_removes_unknown_files_and_keeps_preserved_paths() {
        let profile_root = test_root("strict_cleanup_removes_unknown_files_and_keeps_preserved_paths");
        let files_root = profile_root.join("files");
        write_test_file(&files_root.join("mods/official.jar"), "official");
        write_test_file(&files_root.join("mods/custom.jar"), "custom");
        write_test_file(&files_root.join("saves/world/level.dat"), "save");
        write_test_file(&files_root.join("options.txt"), "options");

        let hash = hex_hash(Sha256::digest(b"official").as_slice());
        let manifest = test_manifest(
            vec![test_manifest_file("mods/official.jar", &hash, "official".len() as i64)],
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
        let profile_root = test_root("symlink_managed_file_forces_download_and_whitelist_symlink_is_kept");
        let files_root = profile_root.join("files");
        fs::create_dir_all(files_root.join("mods")).unwrap();
        fs::create_dir_all(files_root.join("saves")).unwrap();
        std::os::unix::fs::symlink("/tmp/managed-target", files_root.join("mods/official.jar")).unwrap();
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
        }
    }
}
