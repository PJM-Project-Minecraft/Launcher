use std::collections::HashMap;
use std::marker::PhantomData;
use std::sync::{Arc, Mutex};

use serde::{Deserialize, Serialize};
use serde_json::Value;
use tauri::{AppHandle, Emitter, Manager};

pub type SharedString = String;

#[derive(Debug, Clone, Default, Serialize, Deserialize)]
#[serde(rename_all = "camelCase")]
pub struct NewsItem {
    pub title: String,
    pub date: String,
    pub body: String,
}

#[derive(Debug, Clone, Serialize, Deserialize)]
#[serde(rename_all = "camelCase")]
pub struct DeliveryViewState {
    pub phase: String,
    pub message: String,
    pub version: String,
    pub progress: f32,
    pub mandatory: bool,
    pub retryable: bool,
}

impl Default for DeliveryViewState {
    fn default() -> Self {
        Self {
            phase: "idle".into(),
            message: String::new(),
            version: String::new(),
            progress: 0.0,
            mandatory: false,
            retryable: false,
        }
    }
}

#[derive(Debug, Clone, Serialize)]
#[serde(rename_all = "camelCase")]
pub struct UiState {
    pub launcher_delivery: DeliveryViewState,
    pub profile_delivery: DeliveryViewState,
    pub api_url: String,
    pub server_names: Vec<String>,
    pub server_index: i32,
    pub server_visible: bool,
    pub message: String,
    pub user_login: String,
    pub user_uuid: String,
    pub token_expires_at: String,
    pub login_value: String,
    pub password_value: String,
    pub totp_value: String,
    pub requires_totp: bool,
    pub session_restoring: bool,
    pub is_loading: bool,
    pub is_authenticated: bool,
    pub is_slim: bool,
    pub is_syncing: bool,
    pub has_profile: bool,
    pub download_panel_visible: bool,
    pub selected_profile_name: String,
    pub selected_profile_version: String,
    pub profile_status: String,
    pub playtime_total: String,
    pub download_phase: String,
    pub download_file: String,
    pub download_counter: String,
    pub download_progress: f32,
    pub settings_visible: bool,
    pub memory_gb: i32,
    pub memory_max: i32,
    pub memory_auto: bool,
    pub memory_label: String,
    pub discrete_gpu_available: bool,
    pub discrete_gpu_label: String,
    pub use_discrete_gpu: bool,
    pub discord_rpc_enabled: bool,
    pub install_folder: String,
    pub news_items: Vec<NewsItem>,
    pub anticheat_alert: String,
    pub policy_visible: bool,
    pub policy_accepting: bool,
    pub policy_text: String,
    pub policy_version_label: String,
    pub policy_version: i32,
}

impl Default for UiState {
    fn default() -> Self {
        Self {
            launcher_delivery: DeliveryViewState::default(),
            profile_delivery: DeliveryViewState::default(),
            api_url: String::new(),
            server_names: Vec::new(),
            server_index: 0,
            server_visible: false,
            message: "Запускаем лаунчер…".to_string(),
            user_login: String::new(),
            user_uuid: String::new(),
            token_expires_at: String::new(),
            login_value: String::new(),
            password_value: String::new(),
            totp_value: String::new(),
            requires_totp: false,
            session_restoring: false,
            is_loading: true,
            is_authenticated: false,
            is_slim: false,
            is_syncing: false,
            has_profile: false,
            download_panel_visible: false,
            selected_profile_name: String::new(),
            selected_profile_version: "-".to_string(),
            profile_status: "Offline".to_string(),
            playtime_total: "< 1 мин".to_string(),
            download_phase: String::new(),
            download_file: String::new(),
            download_counter: String::new(),
            download_progress: 0.0,
            settings_visible: false,
            memory_gb: 8,
            memory_max: 64,
            memory_auto: true,
            memory_label: "Авто · 8 ГБ".to_string(),
            discrete_gpu_available: false,
            discrete_gpu_label: String::new(),
            use_discrete_gpu: true,
            discord_rpc_enabled: true,
            install_folder: String::new(),
            news_items: Vec::new(),
            anticheat_alert: String::new(),
            policy_visible: false,
            policy_accepting: false,
            policy_text: String::new(),
            policy_version_label: String::new(),
            policy_version: 0,
        }
    }
}

type Callback = Arc<dyn Fn(Value) + Send + Sync + 'static>;

struct AppInner {
    handle: AppHandle,
    state: Mutex<UiState>,
    callbacks: Mutex<HashMap<&'static str, Callback>>,
}

#[derive(Clone)]
pub struct AppWindow(Arc<AppInner>);

#[derive(Clone)]
pub struct Weak<T> {
    inner: std::sync::Weak<AppInner>,
    marker: PhantomData<T>,
}

impl Weak<AppWindow> {
    pub fn upgrade(&self) -> Option<AppWindow> {
        self.inner.upgrade().map(AppWindow)
    }

    pub fn upgrade_in_event_loop<F>(&self, callback: F) -> Result<(), ()>
    where
        F: FnOnce(AppWindow) + Send + 'static,
    {
        if let Some(app) = self.upgrade() {
            callback(app);
        }
        Ok(())
    }
}

macro_rules! string_setter {
    ($name:ident, $field:ident) => {
        pub fn $name(&self, value: String) {
            self.update(|state| state.$field = value);
        }
    };
}

macro_rules! value_setter {
    ($name:ident, $field:ident, $ty:ty) => {
        pub fn $name(&self, value: $ty) {
            self.update(|state| state.$field = value);
        }
    };
}

impl AppWindow {
    pub fn new(handle: AppHandle) -> Self {
        Self(Arc::new(AppInner {
            handle,
            state: Mutex::new(UiState::default()),
            callbacks: Mutex::new(HashMap::new()),
        }))
    }

    pub fn as_weak(&self) -> Weak<AppWindow> {
        Weak {
            inner: Arc::downgrade(&self.0),
            marker: PhantomData,
        }
    }

    pub fn snapshot(&self) -> UiState {
        self.0
            .state
            .lock()
            .map(|state| state.clone())
            .unwrap_or_default()
    }

    pub fn dispatch(&self, action: &str, payload: Value) -> Result<(), String> {
        let callback = self
            .0
            .callbacks
            .lock()
            .map_err(|_| "Не удалось открыть обработчик интерфейса.".to_string())?
            .get(action)
            .cloned()
            .ok_or_else(|| format!("Неизвестное действие интерфейса: {action}"))?;
        callback(payload);
        Ok(())
    }

    fn update(&self, change: impl FnOnce(&mut UiState)) {
        let snapshot = match self.0.state.lock() {
            Ok(mut state) => {
                change(&mut state);
                state.clone()
            }
            Err(_) => return,
        };
        let _ = self.0.handle.emit("launcher-state", snapshot);
    }

    fn on<F>(&self, name: &'static str, callback: F)
    where
        F: Fn(Value) + Send + Sync + 'static,
    {
        if let Ok(mut callbacks) = self.0.callbacks.lock() {
            callbacks.insert(name, Arc::new(callback));
        }
    }

    pub fn show(&self) -> Result<(), String> {
        self.0
            .handle
            .get_webview_window("main")
            .ok_or_else(|| "Окно лаунчера не найдено.".to_string())?
            .show()
            .map_err(|error| error.to_string())
    }

    pub fn hide(&self) -> Result<(), String> {
        self.0
            .handle
            .get_webview_window("main")
            .ok_or_else(|| "Окно лаунчера не найдено.".to_string())?
            .hide()
            .map_err(|error| error.to_string())
    }

    string_setter!(set_api_url, api_url);
    string_setter!(set_message, message);
    string_setter!(set_user_login, user_login);
    string_setter!(set_user_uuid, user_uuid);
    string_setter!(set_token_expires_at, token_expires_at);
    string_setter!(set_login_value, login_value);
    string_setter!(set_password_value, password_value);
    string_setter!(set_totp_value, totp_value);
    string_setter!(set_selected_profile_name, selected_profile_name);
    string_setter!(set_selected_profile_version, selected_profile_version);
    string_setter!(set_profile_status, profile_status);
    string_setter!(set_playtime_total, playtime_total);
    string_setter!(set_download_phase, download_phase);
    string_setter!(set_download_file, download_file);
    string_setter!(set_download_counter, download_counter);
    string_setter!(set_memory_label, memory_label);
    string_setter!(set_discrete_gpu_label, discrete_gpu_label);
    string_setter!(set_install_folder, install_folder);
    string_setter!(set_anticheat_alert, anticheat_alert);
    string_setter!(set_policy_text, policy_text);
    string_setter!(set_policy_version_label, policy_version_label);

    value_setter!(set_server_index, server_index, i32);
    value_setter!(set_server_visible, server_visible, bool);
    value_setter!(set_requires_totp, requires_totp, bool);
    value_setter!(set_session_restoring, session_restoring, bool);
    value_setter!(set_is_loading, is_loading, bool);
    value_setter!(set_is_authenticated, is_authenticated, bool);
    value_setter!(set_is_slim, is_slim, bool);
    value_setter!(set_is_syncing, is_syncing, bool);
    value_setter!(set_has_profile, has_profile, bool);
    value_setter!(set_download_panel_visible, download_panel_visible, bool);
    value_setter!(set_download_progress, download_progress, f32);
    value_setter!(set_settings_visible, settings_visible, bool);
    value_setter!(set_memory_gb, memory_gb, i32);
    value_setter!(set_memory_max, memory_max, i32);
    value_setter!(set_memory_auto, memory_auto, bool);
    value_setter!(set_discrete_gpu_available, discrete_gpu_available, bool);
    value_setter!(set_use_discrete_gpu, use_discrete_gpu, bool);
    value_setter!(set_discord_rpc_enabled, discord_rpc_enabled, bool);
    value_setter!(set_policy_visible, policy_visible, bool);
    value_setter!(set_policy_accepting, policy_accepting, bool);
    value_setter!(set_policy_version, policy_version, i32);

    pub fn set_server_names(&self, names: Vec<String>) {
        self.update(|state| state.server_names = names);
    }

    pub fn set_server_name(&self, index: usize, label: String) {
        self.update(|state| {
            if let Some(item) = state.server_names.get_mut(index) {
                *item = label;
            }
        });
    }

    pub fn set_news_items(&self, items: Vec<NewsItem>) {
        self.update(|state| state.news_items = items);
    }

    pub fn set_launcher_delivery(&self, state: DeliveryViewState) {
        self.update(|current| current.launcher_delivery = state);
    }

    pub fn set_profile_delivery(&self, state: DeliveryViewState) {
        self.update(|current| current.profile_delivery = state);
    }

    pub fn get_policy_accepting(&self) -> bool {
        self.snapshot().policy_accepting
    }
    pub fn get_policy_version(&self) -> i32 {
        self.snapshot().policy_version
    }
    pub fn get_is_syncing(&self) -> bool {
        self.snapshot().is_syncing
    }
    pub fn get_profile_delivery_phase(&self) -> String {
        self.snapshot().profile_delivery.phase
    }
    pub fn get_policy_text(&self) -> String {
        self.snapshot().policy_text
    }
    pub fn get_is_authenticated(&self) -> bool {
        self.snapshot().is_authenticated
    }

    pub fn on_login_requested<F>(&self, callback: F)
    where
        F: Fn(String, String, String) + Send + Sync + 'static,
    {
        self.on("login", move |payload| {
            let login = payload
                .get("login")
                .and_then(Value::as_str)
                .unwrap_or_default()
                .to_string();
            let password = payload
                .get("password")
                .and_then(Value::as_str)
                .unwrap_or_default()
                .to_string();
            let totp = payload
                .get("totp")
                .and_then(Value::as_str)
                .unwrap_or_default()
                .to_string();
            callback(login, password, totp);
        });
    }

    pub fn on_server_selected<F>(&self, callback: F)
    where
        F: Fn(i32) + Send + Sync + 'static,
    {
        self.on("selectServer", move |payload| {
            callback(payload.get("index").and_then(Value::as_i64).unwrap_or(0) as i32)
        });
    }

    pub fn on_memory_set_requested<F>(&self, callback: F)
    where
        F: Fn(i32) + Send + Sync + 'static,
    {
        self.on("setMemory", move |payload| {
            callback(payload.get("value").and_then(Value::as_i64).unwrap_or(8) as i32)
        });
    }

    pub fn on_discrete_gpu_requested<F>(&self, callback: F)
    where
        F: Fn(bool) + Send + Sync + 'static,
    {
        self.on("setDiscreteGpu", move |payload| {
            callback(
                payload
                    .get("enabled")
                    .and_then(Value::as_bool)
                    .unwrap_or(false),
            )
        });
    }

    pub fn on_discord_rpc_requested<F>(&self, callback: F)
    where
        F: Fn(bool) + Send + Sync + 'static,
    {
        self.on("setDiscordRpc", move |payload| {
            callback(
                payload
                    .get("enabled")
                    .and_then(Value::as_bool)
                    .unwrap_or(false),
            )
        });
    }

    pub fn on_open_url<F>(&self, callback: F)
    where
        F: Fn(String) + Send + Sync + 'static,
    {
        self.on("openUrl", move |payload| {
            callback(
                payload
                    .get("url")
                    .and_then(Value::as_str)
                    .unwrap_or_default()
                    .to_string(),
            )
        });
    }
}

macro_rules! no_arg_callback {
    ($method:ident, $action:literal) => {
        impl AppWindow {
            pub fn $method<F>(&self, callback: F)
            where
                F: Fn() + Send + Sync + 'static,
            {
                self.on($action, move |_| callback());
            }
        }
    };
}

no_arg_callback!(on_anticheat_alert_dismiss, "dismissAnticheatAlert");
no_arg_callback!(on_change_install_folder_requested, "changeInstallFolder");
no_arg_callback!(on_logout_requested, "logout");
no_arg_callback!(on_memory_auto_requested, "memoryAuto");
no_arg_callback!(on_memory_decrease_requested, "memoryDecrease");
no_arg_callback!(on_memory_increase_requested, "memoryIncrease");
no_arg_callback!(on_open_install_folder_requested, "openInstallFolder");
no_arg_callback!(on_open_mods_folder_requested, "openModsFolder");
no_arg_callback!(on_play_requested, "play");
no_arg_callback!(on_policy_accept_requested, "acceptPolicy");
no_arg_callback!(on_settings_close_requested, "closeSettings");
no_arg_callback!(on_settings_requested, "openSettings");
no_arg_callback!(on_update_restart_requested, "restartForUpdate");
no_arg_callback!(on_update_retry_requested, "retryUpdate");

pub fn invoke_from_ui(callback: impl FnOnce() + Send + 'static) -> Result<(), ()> {
    callback();
    Ok(())
}
