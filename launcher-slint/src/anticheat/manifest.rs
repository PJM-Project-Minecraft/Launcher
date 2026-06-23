//! Манифест целостности (`GET /api/anticheat/manifest`): SHA-256 инжектируемых
//! артефактов (agent.jar / нативная библиотека / authlib-injector). Сверяется перед
//! инжектом — несовпадение = подмена. Тянется без auth; None = недоступен (fail-open).

use serde::Deserialize;

use crate::artifacts::sha_opt;
use crate::AppConfig;

#[derive(Debug, Default, Deserialize)]
pub struct IntegrityManifest {
    #[serde(rename = "agentSha256", default)]
    agent_sha256: String,
    #[serde(rename = "authlibSha256", default)]
    authlib_sha256: String,
    #[serde(default)]
    native: NativeSha,
}

#[derive(Debug, Default, Deserialize)]
struct NativeSha {
    #[serde(default)]
    linux: String,
    #[serde(default)]
    windows: String,
}

impl IntegrityManifest {
    /// Тянет манифест с бэкенда (без auth). None — недоступен (оффлайн/сбой):
    /// тогда SHA-сверка не выполняется (fail-open, не ломаем оффлайн-запуск).
    pub fn fetch(config: &AppConfig) -> Option<Self> {
        let client = crate::download_client().ok()?;
        let url = format!(
            "{}/api/anticheat/manifest",
            config.api_url.trim_end_matches('/')
        );
        let response = client.get(url).send().ok()?;
        if !response.status().is_success() {
            return None;
        }
        response.json::<Self>().ok()
    }

    /// Ожидаемый SHA agent.jar (None — нет на бэкенде).
    pub fn agent_sha(&self) -> Option<&str> {
        sha_opt(&self.agent_sha256)
    }

    /// Ожидаемый SHA authlib-injector.jar.
    pub fn authlib_sha(&self) -> Option<&str> {
        sha_opt(&self.authlib_sha256)
    }

    /// Ожидаемый SHA нативной библиотеки для текущей ОС.
    pub fn native_sha(&self) -> Option<&str> {
        let raw = if cfg!(target_os = "linux") {
            &self.native.linux
        } else if cfg!(target_os = "windows") {
            &self.native.windows
        } else {
            ""
        };
        sha_opt(raw)
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    fn manifest(agent: &str, authlib: &str, linux: &str, windows: &str) -> IntegrityManifest {
        IntegrityManifest {
            agent_sha256: agent.to_string(),
            authlib_sha256: authlib.to_string(),
            native: NativeSha {
                linux: linux.to_string(),
                windows: windows.to_string(),
            },
        }
    }

    #[test]
    fn sha_getters_map_empty_to_none() {
        let m = manifest("", "", "", "");
        assert_eq!(m.agent_sha(), None);
        assert_eq!(m.authlib_sha(), None);
        assert_eq!(m.native_sha(), None);
    }

    #[test]
    fn sha_getters_return_values() {
        let m = manifest("aa", "bb", "cc", "dd");
        assert_eq!(m.agent_sha(), Some("aa"));
        assert_eq!(m.authlib_sha(), Some("bb"));
        // На дев-машине/CI (Linux) native_sha берёт linux-поле.
        #[cfg(target_os = "linux")]
        assert_eq!(m.native_sha(), Some("cc"));
    }
}
