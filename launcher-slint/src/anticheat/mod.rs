//! Лаунчер-сторона античита: pre-launch handshake (баны/форс-апдейт), доставка и
//! инжект агентов с контролем целостности, разбор kick после выхода игры. Внешний
//! интерфейс — фасад `LaunchGuard`, инкапсулирующий состояние сессии запуска.

mod agents;
mod handshake;
mod hwid;
mod inject;
mod manifest;
mod scan;
pub mod screenshot;
pub mod kick;

use std::collections::HashSet;
use std::path::PathBuf;
use std::sync::atomic::{AtomicBool, Ordering};
use std::sync::Arc;
use std::thread;
use std::time::Duration;

use crate::AppConfig;
use manifest::IntegrityManifest;

pub use handshake::POLICY_REQUIRED_ERR;

/// Интервал in-game скана процессов во время игры.
const INGAME_SCAN_INTERVAL: Duration = Duration::from_secs(30);

/// Состояние античит-сессии запуска: токен/nonce/challenge от handshake, манифест
/// целостности (тянется один раз), путь kick-файла (заполняется при инжекте), блэклист
/// процессов (для in-game скана во время игры).
pub struct LaunchGuard {
    launch_token: String,
    nonce: String,
    challenge: String,
    manifest: Option<IntegrityManifest>,
    kick_file: Option<PathBuf>,
    blacklist: Vec<handshake::Signature>,
}

impl LaunchGuard {
    /// Pre-launch проверки: скан процессов против блэклиста, HWID, init-handshake и
    /// манифест целостности. Err — запуск заблокирован (бан/форс-апдейт). Сетевые сбои
    /// = fail-open: guard с пустым токеном (агенты не инжектятся, enforcement на join).
    pub fn begin(config: &AppConfig, token: &str) -> Result<Self, String> {
        let blacklist = handshake::fetch_blacklist(config, token).unwrap_or_default();
        let detections = scan::scan_processes(&blacklist);
        let components = hwid::collect_hwid_components();
        let manifest = IntegrityManifest::fetch(config, token);

        match handshake::init(config, token, &components, &detections) {
            handshake::InitOutcome::Allowed {
                token,
                nonce,
                challenge,
            } => Ok(Self {
                launch_token: token,
                nonce,
                challenge,
                manifest,
                kick_file: None,
                blacklist,
            }),
            handshake::InitOutcome::Blocked(reason) => Err(reason),
            handshake::InitOutcome::UpdateRequired(message) => Err(message),
            handshake::InitOutcome::PolicyRequired => Err(handshake::POLICY_REQUIRED_ERR.to_string()),
            // fail-open: недоступность бэкенда не блокирует игрока.
            handshake::InitOutcome::Unavailable => Ok(Self {
                launch_token: String::new(),
                nonce: String::new(),
                challenge: String::new(),
                manifest,
                kick_file: None,
                blacklist,
            }),
        }
    }

    /// nonce связывает игровую сессию с launch-token (для fetch_yggdrasil_session).
    pub fn nonce(&self) -> &str {
        &self.nonce
    }

    /// launch-token античита: по нему лаунчер опрашивает скриншот-запросы и грузит
    /// JPEG. Пустой — античит недоступен (fail-open), опрос в этом случае no-op.
    pub fn launch_token(&self) -> &str {
        &self.launch_token
    }

    /// Ожидаемый SHA authlib-injector.jar (для верифицированной доставки в main).
    pub fn authlib_sha(&self) -> Option<&str> {
        self.manifest.as_ref().and_then(|m| m.authlib_sha())
    }

    /// Инжектирует агентов античита в начало jvm_args (только при наличии launch-token).
    /// Err — подмена артефакта (блок запуска).
    pub fn inject_into(
        &mut self,
        jvm_args: &mut Vec<String>,
        config: &AppConfig,
    ) -> Result<(), String> {
        if self.launch_token.is_empty() {
            return Ok(());
        }
        let plan = inject::build(
            &self.launch_token,
            &self.challenge,
            self.manifest.as_ref(),
            config,
        )?;
        self.kick_file = plan.kick_file;
        jvm_args.splice(0..0, plan.args);
        Ok(())
    }

    /// После выхода игры: причина kick, если агент убил JVM (kick-файл создан).
    /// Сам файл удаляется.
    pub fn finish(&self) -> Option<kick::KickReason> {
        let path = self.kick_file.as_ref()?;
        let reason = kick::KickReason::read_from(path);
        let _ = std::fs::remove_file(path);
        reason
    }
}

/// Запускает фоновый поток in-game скана процессов во время игры: периодически сверяет
/// запущенные процессы с блэклистом и шлёт НОВЫЕ совпадения на бэкенд по launch-token.
/// poll_until — общий скелет polling-цикла для фоновых задач лаунчера (keepalive,
/// in-game скан, опрос скриншот-запросов). Спит короткими квантами для отзывчивого
/// завершения на закрытии игры (stop), вызывает work каждые interval. Единый
/// источник истины для терминации/интервала — иначе три копии цикла разойдутся.
pub fn poll_until(stop: &AtomicBool, interval: Duration, mut work: impl FnMut()) {
    let quantum = Duration::from_secs(2);
    let mut elapsed = Duration::ZERO;
    loop {
        thread::sleep(quantum);
        if stop.load(Ordering::Relaxed) {
            return;
        }
        elapsed += quantum;
        if elapsed < interval {
            continue;
        }
        elapsed = Duration::ZERO;
        work();
    }
}

/// Закрывает пробел pre-launch скана — чит-софт, запущенный уже ПОСЛЕ старта игры.
/// No-op (поток сразу завершается) без launch-token или при пустом блэклисте. Данные
/// клонируются в поток, поэтому guard остаётся доступен в основном потоке (для finish).
pub fn spawn_ingame_scan(
    config: &AppConfig,
    guard: &LaunchGuard,
    stop: Arc<AtomicBool>,
) -> thread::JoinHandle<()> {
    let api_url = config.api_url();
    let launch_token = guard.launch_token.clone();
    let blacklist = guard.blacklist.clone();
    thread::spawn(move || {
        if launch_token.is_empty() || blacklist.is_empty() {
            return;
        }
        let mut reported: HashSet<String> = HashSet::new();
        poll_until(&stop, INGAME_SCAN_INTERVAL, || {
            for d in scan::scan_processes(&blacklist) {
                // Дедуп: одну и ту же сигнатуру за сессию шлём один раз.
                if reported.insert(d.signature.clone()) {
                    scan::report_detection(&api_url, &launch_token, &d);
                }
            }
        });
    })
}
