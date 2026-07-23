//! Декларативная сборка JVM-аргументов инжекта агентов античита. Чистые функции
//! сборки строк (`native_args`/`agent_args`) тестируются юнитами; `build` оркеструет
//! доставку артефактов (artifacts через agents) и чистку флаг-файлов прошлой сессии.

use std::fs;
use std::path::{Path, PathBuf};

use obfstr::obfstr;

use crate::anticheat::{agents, manifest::IntegrityManifest};
use crate::AppConfig;

// Контракт флаг-файлов с агентами (создаются рядом с артефактами в папке данных).
const NATIVE_FLAG: &str = "ac_native.flag";
const KICK_FLAG: &str = "ac_kick.flag";

/// Готовый план инжекта: аргументы для добавления в начало jvm_args + путь kick-файла.
pub struct InjectionPlan {
    pub args: Vec<String>,
    pub kick_file: Option<PathBuf>,
}

/// Аргументы нативного JVMTI-агента: запрет позднего attach + flag-файл + agentpath.
fn native_args(native_path: &Path, flag_path: &Path) -> Vec<String> {
    vec![
        obfstr!("-XX:+DisableAttachMechanism").to_string(),
        format!("{}{}", obfstr!("-Dac.native.flag="), flag_path.to_string_lossy()),
        format!(
            "{}{}={}",
            obfstr!("-agentpath:"),
            native_path.to_string_lossy(),
            flag_path.to_string_lossy()
        ),
    ]
}

/// Аргументы Java-агента: токен, URL бэкенда, kick-файл, attestation-challenge, javaagent.
fn agent_args(
    token: &str,
    api_url: &str,
    kick_path: &Path,
    challenge: &str,
    agent_path: &Path,
) -> Vec<String> {
    vec![
        format!("{}{}", obfstr!("-Dac.token="), token),
        format!("{}{}", obfstr!("-Dac.url="), api_url),
        format!("{}{}", obfstr!("-Dac.kickfile="), kick_path.to_string_lossy()),
        format!("{}{}", obfstr!("-Dac.challenge="), challenge),
        format!("{}{}", obfstr!("-javaagent:"), agent_path.to_string_lossy()),
    ]
}

/// Собирает план инжекта: доставка нативного и Java-агента (с SHA-сверкой через
/// agents/artifacts), чистка флаг/events/kick файлов прошлой сессии, сборка аргументов
/// (сначала нативные, затем Java-агента). Err — подмена артефакта (блок запуска).
pub fn build(
    token: &str,
    challenge: &str,
    manifest: Option<&IntegrityManifest>,
    config: &AppConfig,
) -> Result<InjectionPlan, String> {
    let mut args = Vec::new();
    let mut kick_file = None;

    // Нативный JVMTI-агент (M4): anti-inject/anti-debug + flag-файл для Java-агента.
    let native_sha = manifest.and_then(|m| m.native_sha());
    if let Some(native) = agents::ensure_native(config, native_sha)? {
        let flag = native.with_file_name(NATIVE_FLAG);
        let _ = fs::remove_file(&flag); // свежий старт: убираем прошлый флаг
        // КРИТИЧНО: чистим и файл событий, иначе Java-поллер при новом запуске
        // перечитает старые детекты прошлой (читерской) сессии и кикнет чистую игру.
        let _ = fs::remove_file(native.with_file_name(format!("{}.events", NATIVE_FLAG)));
        args.extend(native_args(&native, &flag));
    }

    // Java-агент (M3): confirm + рантайм-скан классов/модов + heartbeat.
    let agent_sha = manifest.and_then(|m| m.agent_sha());
    if let Some(agent) = agents::ensure_agent(config, agent_sha)? {
        let kick = agent.with_file_name(KICK_FLAG);
        let _ = fs::remove_file(&kick); // свежий старт
        let api_url = config.api_url();
        args.extend(agent_args(
            token,
            api_url.trim_end_matches('/'),
            &kick,
            challenge,
            &agent,
        ));
        kick_file = Some(kick);
    }

    Ok(InjectionPlan { args, kick_file })
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn native_args_contain_guards() {
        let a = native_args(Path::new("/d/lib.so"), Path::new("/d/ac_native.flag"));
        assert!(a.iter().any(|s| s == "-XX:+DisableAttachMechanism"));
        assert!(a.iter().any(|s| s.contains("-Dac.native.flag=/d/ac_native.flag")));
        assert!(a
            .iter()
            .any(|s| s.contains("-agentpath:/d/lib.so=/d/ac_native.flag")));
    }

    #[test]
    fn agent_args_contain_all_props() {
        let a = agent_args(
            "tok",
            "https://x.test",
            Path::new("/d/ac_kick.flag"),
            "chal",
            Path::new("/d/agent.jar"),
        );
        assert!(a.iter().any(|s| s == "-Dac.token=tok"));
        assert!(a.iter().any(|s| s == "-Dac.url=https://x.test"));
        assert!(a.iter().any(|s| s.contains("-Dac.kickfile=/d/ac_kick.flag")));
        assert!(a.iter().any(|s| s == "-Dac.challenge=chal"));
        assert!(a.iter().any(|s| s.contains("-javaagent:/d/agent.jar")));
    }
}
