//! Лаунчер-сторона античита: pre-launch handshake (баны/форс-апдейт), доставка и
//! инжект агентов с контролем целостности, разбор kick после выхода игры. Внешний
//! интерфейс — фасад `LaunchGuard`, инкапсулирующий состояние сессии запуска.

mod agents;
mod handshake;
mod hwid;
mod inject;
mod manifest;
mod scan;
pub mod kick;

use std::path::PathBuf;

use crate::AppConfig;
use manifest::IntegrityManifest;

/// Состояние античит-сессии запуска: токен/nonce/challenge от handshake, манифест
/// целостности (тянется один раз), путь kick-файла (заполняется при инжекте).
pub struct LaunchGuard {
    launch_token: String,
    nonce: String,
    challenge: String,
    manifest: Option<IntegrityManifest>,
    kick_file: Option<PathBuf>,
}

impl LaunchGuard {
    /// Pre-launch проверки: скан процессов против блэклиста, HWID, init-handshake и
    /// манифест целостности. Err — запуск заблокирован (бан/форс-апдейт). Сетевые сбои
    /// = fail-open: guard с пустым токеном (агенты не инжектятся, enforcement на join).
    pub fn begin(config: &AppConfig, token: &str) -> Result<Self, String> {
        let blacklist = handshake::fetch_blacklist(config, token).unwrap_or_default();
        let detections = scan::scan_processes(&blacklist);
        let hwid_hash = hwid::collect_hwid_hash();
        let manifest = IntegrityManifest::fetch(config);

        match handshake::init(config, token, &hwid_hash, &detections) {
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
            }),
            handshake::InitOutcome::Blocked(reason) => Err(reason),
            handshake::InitOutcome::UpdateRequired(message) => Err(message),
            // fail-open: недоступность бэкенда не блокирует игрока.
            handshake::InitOutcome::Unavailable => Ok(Self {
                launch_token: String::new(),
                nonce: String::new(),
                challenge: String::new(),
                manifest,
                kick_file: None,
            }),
        }
    }

    /// nonce связывает игровую сессию с launch-token (для fetch_yggdrasil_session).
    pub fn nonce(&self) -> &str {
        &self.nonce
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
