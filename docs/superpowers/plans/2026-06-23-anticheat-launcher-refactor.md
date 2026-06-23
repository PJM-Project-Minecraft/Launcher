# Рефакторинг античита launcher-slint — Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Собрать разбросанную по `main.rs` античит-логику в модуль `anticheat/` с фасадом `LaunchGuard`, выделить общий верифицированный загрузчик `artifacts`, сохранив поведение enforcement (fail-open).

**Architecture:** Снизу вверх. Сначала общий загрузчик `artifacts.rs`, затем листовые подмодули античита (`manifest`, `agents`, `kick`, `inject`), затем фасад `LaunchGuard` (+ `handshake.rs`), затем интеграция `launch_profile`. Каждая задача оставляет код компилируемым и тесты зелёными.

**Tech Stack:** Rust 2021, reqwest blocking, serde, sha2. UI на Slint (не трогаем). Пакет `launcher-slint`.

## Global Constraints

- Все правки внутри `/home/liko/Разработка/Launcher/launcher-slint/`.
- **fail-open сохраняется** во всех точках: недоступность бэкенда/манифеста/артефакта НЕ блокирует запуск. Блокируют только: бан (handshake `Blocked`), форс-апдейт (426 `UpdateRequired`), подмена артефакта (`IntegrityError::Tampered`).
- Заголовок `X-Launcher-Version` в handshake/init = `env!("CARGO_PKG_VERSION")` — сохранить дословно.
- Набор JVM-аргументов инжекта функционально эквивалентен оригиналу. Единственное допустимое отклонение: внутренний порядок античит-флагов становится «native-флаги, затем agent-флаги».
- **Без новых runtime-зависимостей.** Юнит-тесты — только на чистых функциях (без сети/ФС). tempfile не добавляем.
- Команды проверки (из `launcher-slint/`): `cargo build`, `cargo test`, `cargo clippy --all-targets -- -D warnings`.
- Стиль: комментарии на русском, как в существующем коде. Сообщения об ошибках игроку — на русском, дословно сохранять существующие тексты.

---

## File Structure

```
src/
  artifacts.rs          ← НОВЫЙ top-level: verify_sha / ensure / IntegrityError / sha_opt
  anticheat/
    mod.rs              ← фасад LaunchGuard; объявление подмодулей
    handshake.rs        ← НОВЫЙ: init (InitOutcome) / confirm / fetch_blacklist / Signature
    hwid.rs             ← без изменений
    scan.rs             ← без изменений
    manifest.rs         ← НОВЫЙ: IntegrityManifest (fetch + agent_sha/native_sha/authlib_sha)
    agents.rs           ← НОВЫЙ: ensure_agent / ensure_native / native_agent_target
    inject.rs           ← НОВЫЙ: native_args / agent_args / InjectionPlan / build
    kick.rs             ← НОВЫЙ: KICK_PREFIX / KickReason (parse / read_from / into_alert)
  main.rs               ← −≈250 строк античита; launch_profile через LaunchGuard
```

---

## Task 1: Общий верифицированный загрузчик `artifacts.rs`

**Files:**
- Create: `src/artifacts.rs`
- Modify: `src/main.rs` — добавить `mod artifacts;`; удалить `verify_sha` (1385–1396), `download_and_verify` (1401–1429), `ensure_artifact` (1434–1464), `sha_opt` (1375–1381); переключить `ensure_authlib_injector`, `ensure_agent_jar`, `ensure_native_agent` на `artifacts::ensure`.

**Interfaces:**
- Consumes: `crate::hash_file(&Path) -> Result<String, String>` (остаётся в main.rs), `crate::download_client() -> Result<Client, String>` (остаётся в main.rs).
- Produces:
  - `pub fn artifacts::sha_opt(s: &str) -> Option<&str>`
  - `pub enum artifacts::IntegrityError { Tampered(String) }` + `pub fn message(&self) -> String`
  - `pub fn artifacts::verify_sha(path: &Path, expected: Option<&str>) -> Result<(), IntegrityError>`
  - `pub fn artifacts::ensure(client: &Client, url: &str, path: &Path, dir: &Path, expected: Option<&str>) -> Result<Option<PathBuf>, IntegrityError>`

- [ ] **Step 1: Создать `src/artifacts.rs` с полным содержимым**

```rust
//! Верифицированный загрузчик артефактов: скачивание с SHA-256-сверкой, до 2 попыток
//! при подмене, откат на кэш. Общий для agent.jar / нативной библиотеки /
//! authlib-injector — НЕ зависит от модуля anticheat.

use std::fs;
use std::path::{Path, PathBuf};

use reqwest::blocking::Client;

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
    client: &Client,
    url: &str,
    path: &Path,
    dir: &Path,
    expected: Option<&str>,
) -> Result<bool, IntegrityError> {
    if fs::create_dir_all(dir).is_err() {
        return Ok(false);
    }
    let Ok(response) = client.get(url).send() else {
        return Ok(false);
    };
    if !response.status().is_success() {
        return Ok(false);
    }
    let Ok(bytes) = response.bytes() else {
        return Ok(false);
    };
    let tmp = path.with_extension("part");
    if fs::write(&tmp, &bytes).is_err() {
        return Ok(false);
    }
    if fs::rename(&tmp, path).is_err() {
        let _ = fs::remove_file(&tmp);
        return Ok(false);
    }
    verify_sha(path, expected).map(|_| true)
}

/// Скачивает артефакт с SHA-сверкой (до 2 попыток при подмене/битой загрузке), с
/// откатом на кэш. Ok(Some) — готов и валиден; Ok(None) — недоступен (оффлайн без
/// кэша, fail-open); Err — подмена (блок запуска).
pub fn ensure(
    client: &Client,
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

    #[test]
    fn verify_sha_mismatch_is_tampered() {
        // Несуществующий файл + ожидаемый хэш → ошибка чтения трактуется как подмена.
        let err = verify_sha(Path::new("/nonexistent/agent.jar"), Some("deadbeef"));
        assert!(matches!(err, Err(IntegrityError::Tampered(_))));
    }
}
```

- [ ] **Step 2: Запустить тесты модуля — убедиться, что компилируется и проходит**

Прежде нужно подключить модуль (иначе тесты не видны). Добавить в `src/main.rs` после строки `mod anticheat;` (строка 23):

```rust
mod artifacts;
```

Run: `cd /home/liko/Разработка/Launcher/launcher-slint && cargo test artifacts:: 2>&1 | tail -20`
Expected: FAIL компиляции — в `main.rs` всё ещё есть старые `verify_sha`/`ensure_artifact`/`sha_opt` (конфликт имён при вызовах) ИЛИ дублирование. Это ожидаемо до Step 3.

- [ ] **Step 3: Удалить перенесённые функции из `main.rs` и переключить вызывателей**

Удалить из `main.rs`: `sha_opt` (1375–1381), `verify_sha` (1385–1396), `download_and_verify` (1401–1429), `ensure_artifact` (1434–1464).

Переписать `ensure_authlib_injector` (1466–1479) — теперь поверх `artifacts::ensure`:

```rust
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
        config.api_url.trim_end_matches('/')
    );
    let client = download_client()?;
    artifacts::ensure(&client, &url, &path, &dir, expected_sha).map_err(|e| e.message())
}
```

Переписать `ensure_agent_jar` (1484–1497):

```rust
fn ensure_agent_jar(
    config: &AppConfig,
    expected_sha: Option<&str>,
) -> Result<Option<PathBuf>, String> {
    let Some(dir) = project_dirs().ok().map(|d| d.data_dir().to_path_buf()) else {
        return Ok(None);
    };
    let path = dir.join("anticheat-agent.jar");
    let url = format!(
        "{}/api/anticheat/agent.jar",
        config.api_url.trim_end_matches('/')
    );
    let client = download_client()?;
    artifacts::ensure(&client, &url, &path, &dir, expected_sha).map_err(|e| e.message())
}
```

Переписать `ensure_native_agent` (1515–1532):

```rust
fn ensure_native_agent(
    config: &AppConfig,
    expected_sha: Option<&str>,
) -> Result<Option<PathBuf>, String> {
    let Some((os_token, file_name)) = native_agent_target() else {
        return Ok(None);
    };
    let Some(dir) = project_dirs().ok().map(|d| d.data_dir().to_path_buf()) else {
        return Ok(None);
    };
    let path = dir.join(file_name);
    let url = format!(
        "{}/api/anticheat/native/{}",
        config.api_url.trim_end_matches('/'),
        os_token
    );
    let client = download_client()?;
    artifacts::ensure(&client, &url, &path, &dir, expected_sha).map_err(|e| e.message())
}
```

Теперь в `main.rs` остаётся вызыватель `sha_opt` в `launch_profile` (строки 2166, 2187, 2203) и `fetch_anticheat_manifest`. Они используют локальный `sha_opt`, который мы удалили. Временно заменить эти три вызова на `artifacts::sha_opt(...)`:
- 2166: `let authlib_sha = ac_manifest.as_ref().and_then(|m| artifacts::sha_opt(&m.authlib_sha256));`
- 2187: `let native_sha = ac_manifest.as_ref().and_then(|m| artifacts::sha_opt(m.native_sha()));`
- 2203: `let agent_sha = ac_manifest.as_ref().and_then(|m| artifacts::sha_opt(&m.agent_sha256));`

- [ ] **Step 4: Сборка, тесты, clippy**

Run: `cd /home/liko/Разработка/Launcher/launcher-slint && cargo build 2>&1 | tail -20 && cargo test 2>&1 | tail -25 && cargo clippy --all-targets -- -D warnings 2>&1 | tail -20`
Expected: build OK, все тесты PASS (включая новые `artifacts::tests`), clippy без предупреждений.

- [ ] **Step 5: Commit**

```bash
cd /home/liko/Разработка/Launcher && git add launcher-slint/src/artifacts.rs launcher-slint/src/main.rs && git commit -m "$(cat <<'EOF'
refactor(launcher): выделить общий верифицированный загрузчик artifacts

verify_sha/download/ensure + IntegrityError вынесены из main.rs в artifacts.rs
с DI HTTP-клиента. Вызыватели (authlib/agent/native) переключены на artifacts::ensure.

Co-Authored-By: Claude Opus 4.8 (1M context) <noreply@anthropic.com>
EOF
)"
```

---

## Task 2: Манифест целостности `anticheat/manifest.rs`

**Files:**
- Create: `src/anticheat/manifest.rs`
- Modify: `src/anticheat/mod.rs` — добавить `mod manifest;` (объявление); реэкспорт не нужен (доступ через `crate::anticheat::manifest`).
- Modify: `src/main.rs` — удалить `AnticheatManifest`/`AnticheatNative`/`impl AnticheatManifest`/`fetch_anticheat_manifest` (1328–1372); переключить `launch_profile` на `anticheat::manifest::IntegrityManifest`.

**Interfaces:**
- Consumes: `crate::AppConfig`, `crate::download_client`, `crate::artifacts::sha_opt`.
- Produces:
  - `pub struct anticheat::manifest::IntegrityManifest`
  - `IntegrityManifest::fetch(&AppConfig) -> Option<Self>`
  - `IntegrityManifest::agent_sha(&self) -> Option<&str>`
  - `IntegrityManifest::native_sha(&self) -> Option<&str>`
  - `IntegrityManifest::authlib_sha(&self) -> Option<&str>`

- [ ] **Step 1: Создать `src/anticheat/manifest.rs`**

```rust
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
```

- [ ] **Step 2: Подключить модуль и запустить тесты (ожидаем конфликт до Step 3)**

Добавить в `src/anticheat/mod.rs` рядом с `mod hwid;`/`mod scan;`:

```rust
mod manifest;
```

Run: `cd /home/liko/Разработка/Launcher/launcher-slint && cargo test anticheat::manifest:: 2>&1 | tail -20`
Expected: FAIL компиляции — в `main.rs` ещё есть `AnticheatManifest` и `fetch_anticheat_manifest`, `launch_profile` использует их. Чиним в Step 3.

- [ ] **Step 3: Удалить старое из `main.rs`, переключить `launch_profile`**

Удалить из `main.rs`: `AnticheatManifest` (1328–1336), `AnticheatNative` (1338–1344), `impl AnticheatManifest` (1346–1357), `fetch_anticheat_manifest` (1361–1372).

В `launch_profile` заменить строку 2160:

```rust
    // было: let ac_manifest = fetch_anticheat_manifest(config);
    let ac_manifest = anticheat::manifest::IntegrityManifest::fetch(config);
```

Заменить три места выбора SHA (после переключения в Task 1 они были `artifacts::sha_opt(&m.field)`):

```rust
    // 2166:
    let authlib_sha = ac_manifest.as_ref().and_then(|m| m.authlib_sha());
    // 2187:
    let native_sha = ac_manifest.as_ref().and_then(|m| m.native_sha());
    // 2203:
    let agent_sha = ac_manifest.as_ref().and_then(|m| m.agent_sha());
```

- [ ] **Step 4: Сборка, тесты, clippy**

Run: `cd /home/liko/Разработка/Launcher/launcher-slint && cargo build 2>&1 | tail -20 && cargo test 2>&1 | tail -25 && cargo clippy --all-targets -- -D warnings 2>&1 | tail -20`
Expected: build OK, тесты PASS (включая `anticheat::manifest::tests`), clippy чисто.

- [ ] **Step 5: Commit**

```bash
cd /home/liko/Разработка/Launcher && git add launcher-slint/src/anticheat/manifest.rs launcher-slint/src/anticheat/mod.rs launcher-slint/src/main.rs && git commit -m "$(cat <<'EOF'
refactor(launcher): манифест целостности → anticheat/manifest.rs

IntegrityManifest с fetch + agent_sha/native_sha/authlib_sha. launch_profile
использует методы вместо ручного sha_opt по полям.

Co-Authored-By: Claude Opus 4.8 (1M context) <noreply@anthropic.com>
EOF
)"
```

---

## Task 3: Управление агентами `anticheat/agents.rs`

**Files:**
- Create: `src/anticheat/agents.rs`
- Modify: `src/anticheat/mod.rs` — `mod agents;`.
- Modify: `src/main.rs` — удалить `native_agent_target` (1502–1510), `ensure_agent_jar` (1484–1497 после правок Task 1), `ensure_native_agent` (1515–1532 после правок Task 1); переключить `launch_profile` на `anticheat::agents::*`.

**Interfaces:**
- Consumes: `crate::AppConfig`, `crate::download_client`, `crate::project_dirs`, `crate::artifacts::ensure`.
- Produces:
  - `pub fn agents::ensure_agent(&AppConfig, Option<&str>) -> Result<Option<PathBuf>, String>`
  - `pub fn agents::ensure_native(&AppConfig, Option<&str>) -> Result<Option<PathBuf>, String>`

- [ ] **Step 1: Создать `src/anticheat/agents.rs`**

```rust
//! Доставка артефактов античита в служебную папку данных (через artifacts::ensure
//! с SHA-сверкой): Java-агент (agent.jar) и нативная JVMTI-библиотека по текущей ОС.
//! Err — подмена (блок запуска); Ok(None) — недоступно (fail-open оффлайн).

use std::path::PathBuf;

use crate::AppConfig;

// Имя нативной JVMTI-библиотеки и токен ОС для эндпоинта раздачи. None — ОС без
// собранной нативной части (агент тогда не инжектится, его отсутствие зафиксирует
// Java-агент как детект).
fn native_target() -> Option<(&'static str, &'static str)> {
    if cfg!(target_os = "linux") {
        Some(("linux", "libanticheat.so"))
    } else if cfg!(target_os = "windows") {
        Some(("windows", "anticheat.dll"))
    } else {
        None
    }
}

/// Гарантирует наличие agent.jar античита (SHA-256 из манифеста). Качает свежий jar,
/// при ошибке — кэш. Err — подмена (блок); Ok(None) — недоступно (fail-open).
pub fn ensure_agent(
    config: &AppConfig,
    expected_sha: Option<&str>,
) -> Result<Option<PathBuf>, String> {
    let Some(dir) = crate::project_dirs().ok().map(|d| d.data_dir().to_path_buf()) else {
        return Ok(None);
    };
    let path = dir.join("anticheat-agent.jar");
    let url = format!(
        "{}/api/anticheat/agent.jar",
        config.api_url.trim_end_matches('/')
    );
    let client = crate::download_client()?;
    crate::artifacts::ensure(&client, &url, &path, &dir, expected_sha).map_err(|e| e.message())
}

/// Гарантирует наличие нативной библиотеки античита по текущей ОС (SHA-сверка).
/// Err — подмена (блок); Ok(None) — нет нативной части для ОС или недоступна (fail-open).
pub fn ensure_native(
    config: &AppConfig,
    expected_sha: Option<&str>,
) -> Result<Option<PathBuf>, String> {
    let Some((os_token, file_name)) = native_target() else {
        return Ok(None);
    };
    let Some(dir) = crate::project_dirs().ok().map(|d| d.data_dir().to_path_buf()) else {
        return Ok(None);
    };
    let path = dir.join(file_name);
    let url = format!(
        "{}/api/anticheat/native/{}",
        config.api_url.trim_end_matches('/'),
        os_token
    );
    let client = crate::download_client()?;
    crate::artifacts::ensure(&client, &url, &path, &dir, expected_sha).map_err(|e| e.message())
}
```

- [ ] **Step 2: Подключить модуль (ожидаем конфликт до Step 3)**

Добавить в `src/anticheat/mod.rs`:

```rust
mod agents;
```

Run: `cd /home/liko/Разработка/Launcher/launcher-slint && cargo build 2>&1 | tail -20`
Expected: FAIL — дублирование `ensure_agent_jar`/`ensure_native_agent`/`native_agent_target` ещё в `main.rs`. Чиним в Step 3.

- [ ] **Step 3: Удалить старое из `main.rs`, переключить `launch_profile`**

Удалить из `main.rs`: `ensure_agent_jar`, `native_agent_target`, `ensure_native_agent`.

В `launch_profile` заменить вызовы (строки ~2188 и ~2204):

```rust
    // было: if let Some(native) = ensure_native_agent(config, native_sha)? {
    if let Some(native) = anticheat::agents::ensure_native(config, native_sha)? {
    // ...
    // было: if let Some(agent) = ensure_agent_jar(config, agent_sha)? {
    if let Some(agent) = anticheat::agents::ensure_agent(config, agent_sha)? {
```

- [ ] **Step 4: Сборка, тесты, clippy**

Run: `cd /home/liko/Разработка/Launcher/launcher-slint && cargo build 2>&1 | tail -20 && cargo test 2>&1 | tail -25 && cargo clippy --all-targets -- -D warnings 2>&1 | tail -20`
Expected: build OK, тесты PASS, clippy чисто.

- [ ] **Step 5: Commit**

```bash
cd /home/liko/Разработка/Launcher && git add launcher-slint/src/anticheat/agents.rs launcher-slint/src/anticheat/mod.rs launcher-slint/src/main.rs && git commit -m "$(cat <<'EOF'
refactor(launcher): доставка агентов → anticheat/agents.rs

ensure_agent/ensure_native + native_target вынесены из main.rs поверх artifacts::ensure.

Co-Authored-By: Claude Opus 4.8 (1M context) <noreply@anthropic.com>
EOF
)"
```

---

## Task 4: Обработка kick `anticheat/kick.rs`

**Files:**
- Create: `src/anticheat/kick.rs`
- Modify: `src/anticheat/mod.rs` — `pub mod kick;` (публичный — `KICK_PREFIX` нужен в main.rs).
- Modify: `src/main.rs` — удалить `ANTICHEAT_KICK_PREFIX` (36) и `anticheat_kick_message` (2302–2313); переключить play-handler (810) и чтение kick-файла в `launch_profile` (2286–2296).

**Interfaces:**
- Produces:
  - `pub const anticheat::kick::KICK_PREFIX: &str`
  - `pub struct anticheat::kick::KickReason`
  - `KickReason::parse(&str) -> Option<KickReason>`
  - `KickReason::read_from(&Path) -> Option<KickReason>`
  - `KickReason::into_alert(self) -> String`

- [ ] **Step 1: Написать `src/anticheat/kick.rs` с тестами вперёд (TDD)**

```rust
//! Обработка kick: при детекте Java-агент пишет причину в kick-файл и убивает JVM.
//! Лаунчер читает файл после выхода игры и показывает полноэкранное уведомление.

use std::path::Path;

/// Маркер в начале текста ошибки запуска: означает, что игру закрыл античит.
/// Play-хендлер показывает по нему полноэкранное уведомление вместо обычной ошибки.
pub const KICK_PREFIX: &str = "\u{1}ANTICHEAT_KICK\u{1}";

/// Причина kick, прочитанная из kick-файла.
pub struct KickReason(String);

impl KickReason {
    /// Парсит причину из содержимого kick-файла (строка вида `reason=<код>`).
    /// None — нет строки `reason=`.
    pub fn parse(content: &str) -> Option<Self> {
        let reason = content
            .lines()
            .find_map(|l| l.strip_prefix("reason="))?
            .trim();
        Some(KickReason(reason.to_string()))
    }

    /// Читает kick-файл и парсит причину. None — файла нет / нечитаем / без `reason=`.
    pub fn read_from(path: &Path) -> Option<Self> {
        let content = std::fs::read_to_string(path).ok()?;
        Self::parse(&content)
    }

    /// Текст уведомления игроку (с KICK_PREFIX в начале — по нему UI отличает kick).
    pub fn into_alert(self) -> String {
        let detail = match self.0.as_str() {
            "illegal-class-name" => "обнаружена инъекция стороннего кода (чит-клиент)",
            "inject" => "обнаружена инъекция стороннего кода",
            "" => "обнаружена попытка вмешательства",
            other => other,
        };
        format!(
            "{}⛔ Игра закрыта системой защиты: {}. Уберите сторонние программы и запустите снова.",
            KICK_PREFIX, detail
        )
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn parse_extracts_reason() {
        let r = KickReason::parse("foo=1\nreason=inject\nbar=2").unwrap();
        assert_eq!(r.0, "inject");
    }

    #[test]
    fn parse_without_reason_is_none() {
        assert!(KickReason::parse("garbage\nno reason here").is_none());
    }

    #[test]
    fn alert_has_prefix_and_maps_known_reasons() {
        let a = KickReason("illegal-class-name".to_string()).into_alert();
        assert!(a.starts_with(KICK_PREFIX));
        assert!(a.contains("чит-клиент"));

        assert!(KickReason("inject".to_string())
            .into_alert()
            .contains("инъекция стороннего кода"));
        assert!(KickReason(String::new())
            .into_alert()
            .contains("попытка вмешательства"));
        // Неизвестная причина проходит как есть.
        assert!(KickReason("custom-x".to_string())
            .into_alert()
            .contains("custom-x"));
    }
}
```

- [ ] **Step 2: Подключить модуль и запустить тесты (ожидаем конфликт до Step 3)**

Добавить в `src/anticheat/mod.rs`:

```rust
pub mod kick;
```

Run: `cd /home/liko/Разработка/Launcher/launcher-slint && cargo test anticheat::kick:: 2>&1 | tail -20`
Expected: FAIL компиляции — `main.rs` ещё содержит `ANTICHEAT_KICK_PREFIX`/`anticheat_kick_message`. Чиним в Step 3.

- [ ] **Step 3: Удалить старое из `main.rs`, переключить вызывателей**

Удалить из `main.rs`: `const ANTICHEAT_KICK_PREFIX` (34–36), `fn anticheat_kick_message` (2301–2313).

В play-handler (строка 810) заменить `ANTICHEAT_KICK_PREFIX` на `anticheat::kick::KICK_PREFIX`:

```rust
    if let Some(alert) = message.strip_prefix(anticheat::kick::KICK_PREFIX) {
```

В `launch_profile` заменить блок чтения kick-файла (2286–2296):

```rust
    // было: if let Some(kick) = kick_file { read + parse reason + return Err(prefix+msg) }
    if let Some(kick) = &kick_file {
        if let Some(reason) = anticheat::kick::KickReason::read_from(kick) {
            let _ = fs::remove_file(kick);
            return Err(reason.into_alert());
        }
    }
```

(Примечание: `kick_file` здесь — локальная `Option<PathBuf>` из инжект-блока; в Task 6 уедет в `LaunchGuard`.)

- [ ] **Step 4: Сборка, тесты, clippy**

Run: `cd /home/liko/Разработка/Launcher/launcher-slint && cargo build 2>&1 | tail -20 && cargo test 2>&1 | tail -25 && cargo clippy --all-targets -- -D warnings 2>&1 | tail -20`
Expected: build OK, тесты PASS (включая `anticheat::kick::tests`), clippy чисто.

- [ ] **Step 5: Commit**

```bash
cd /home/liko/Разработка/Launcher && git add launcher-slint/src/anticheat/kick.rs launcher-slint/src/anticheat/mod.rs launcher-slint/src/main.rs && git commit -m "$(cat <<'EOF'
refactor(launcher): обработка kick → anticheat/kick.rs

KICK_PREFIX + KickReason(parse/read_from/into_alert) с юнит-тестами.
play-handler и launch_profile используют новый тип.

Co-Authored-By: Claude Opus 4.8 (1M context) <noreply@anthropic.com>
EOF
)"
```

---

## Task 5: Декларативная сборка инжекта `anticheat/inject.rs`

**Files:**
- Create: `src/anticheat/inject.rs`
- Modify: `src/anticheat/mod.rs` — `mod inject;`.
- Modify: `src/main.rs` — заменить инжект-блок в `launch_profile` (2178–2214) на вызов `inject::build` + splice.

**Interfaces:**
- Consumes: `crate::AppConfig`, `anticheat::agents::{ensure_agent, ensure_native}`, `anticheat::manifest::IntegrityManifest`.
- Produces:
  - `pub struct anticheat::inject::InjectionPlan { pub args: Vec<String>, pub kick_file: Option<PathBuf> }`
  - `pub fn inject::build(token: &str, challenge: &str, manifest: Option<&IntegrityManifest>, config: &AppConfig) -> Result<InjectionPlan, String>`

- [ ] **Step 1: Написать `src/anticheat/inject.rs` с тестами на чистые функции (TDD)**

```rust
//! Декларативная сборка JVM-аргументов инжекта агентов античита. Чистые функции
//! сборки строк (`native_args`/`agent_args`) тестируются юнитами; `build` оркеструет
//! доставку артефактов (artifacts через agents) и чистку флаг-файлов прошлой сессии.

use std::fs;
use std::path::{Path, PathBuf};

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
        "-XX:+DisableAttachMechanism".to_string(),
        format!("-Dac.native.flag={}", flag_path.to_string_lossy()),
        format!(
            "-agentpath:{}={}",
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
        format!("-Dac.token={}", token),
        format!("-Dac.url={}", api_url),
        format!("-Dac.kickfile={}", kick_path.to_string_lossy()),
        format!("-Dac.challenge={}", challenge),
        format!("-javaagent:{}", agent_path.to_string_lossy()),
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
        let api_url = config.api_url.trim_end_matches('/');
        args.extend(agent_args(token, api_url, &kick, challenge, &agent));
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
```

- [ ] **Step 2: Подключить модуль и прогнать тесты чистых функций**

Добавить в `src/anticheat/mod.rs`:

```rust
mod inject;
```

Run: `cd /home/liko/Разработка/Launcher/launcher-slint && cargo test anticheat::inject:: 2>&1 | tail -20`
Expected: PASS оба теста (`native_args_contain_guards`, `agent_args_contain_all_props`). Сборка всего бинаря может ругаться на неиспользуемый `build`, если так — продолжаем к Step 3 (он подключит вызов).

- [ ] **Step 3: Переключить `launch_profile` на `inject::build`**

Заменить в `launch_profile` блок инжекта агентов (текущие строки ~2178–2214, от `let mut kick_file: Option<PathBuf> = None;` до закрывающей `}` блока `if !guard.launch_token.is_empty()`) на:

```rust
    // Инжект агентов античита. Только если handshake/init выдал токен — иначе агенты
    // бессильны, а сессия не пройдёт verified-гейт на join.
    let mut kick_file: Option<PathBuf> = None;
    if !guard.launch_token.is_empty() {
        let plan = anticheat::inject::build(
            &guard.launch_token,
            &guard.challenge,
            ac_manifest.as_ref(),
            config,
        )?;
        kick_file = plan.kick_file;
        jvm_args.splice(0..0, plan.args);
    }
```

Удалить теперь неиспользуемые локальные `let native_sha = ...;` и `let agent_sha = ...;` (если остались отдельными строками вне блока — они были внутри, уже удалены заменой).

- [ ] **Step 4: Сборка, тесты, clippy**

Run: `cd /home/liko/Разработка/Launcher/launcher-slint && cargo build 2>&1 | tail -20 && cargo test 2>&1 | tail -25 && cargo clippy --all-targets -- -D warnings 2>&1 | tail -20`
Expected: build OK, все тесты PASS, clippy чисто.

- [ ] **Step 5: Commit**

```bash
cd /home/liko/Разработка/Launcher && git add launcher-slint/src/anticheat/inject.rs launcher-slint/src/anticheat/mod.rs launcher-slint/src/main.rs && git commit -m "$(cat <<'EOF'
refactor(launcher): декларативная сборка инжекта → anticheat/inject.rs

native_args/agent_args (чистые, юнит-тесты) + build(InjectionPlan). launch_profile
вместо jvm_args.insert(0..4) делает splice готового плана. Порядок флагов:
native, затем agent (для JVM не важен).

Co-Authored-By: Claude Opus 4.8 (1M context) <noreply@anthropic.com>
EOF
)"
```

---

## Task 6: Фасад `LaunchGuard` + `anticheat/handshake.rs` + интеграция

**Files:**
- Create: `src/anticheat/handshake.rs` (перенос тела текущего `mod.rs`)
- Rewrite: `src/anticheat/mod.rs` (фасад `LaunchGuard` + объявления подмодулей)
- Modify: `src/main.rs` — `launch_profile` переходит на `LaunchGuard::begin/nonce/authlib_sha/inject_into/finish`.

**Interfaces:**
- Consumes: `scan::scan_processes`, `hwid::collect_hwid_hash`, `manifest::IntegrityManifest`, `inject::build`, `kick::KickReason`.
- Produces:
  - `pub struct anticheat::LaunchGuard`
  - `LaunchGuard::begin(&AppConfig, &str) -> Result<Self, String>`
  - `LaunchGuard::nonce(&self) -> &str`
  - `LaunchGuard::authlib_sha(&self) -> Option<&str>`
  - `LaunchGuard::inject_into(&mut self, &mut Vec<String>, &AppConfig) -> Result<(), String>`
  - `LaunchGuard::finish(&self) -> Option<kick::KickReason>`

- [ ] **Step 1: Создать `src/anticheat/handshake.rs` (перенос + InitOutcome)**

Перенести из текущего `mod.rs`: `Signature`, `fetch_blacklist`, `confirm`, и переработать `InitResult`/`InitError`/`init_handshake` в `InitOutcome`/`init`:

```rust
//! Pre-launch handshake с бэкендом: blacklist процессов, init (баны/форс-апдейт),
//! confirm (fallback). Явная модель исхода InitOutcome — единственное место, где
//! решается fail-open vs блок.

use serde::Deserialize;

use crate::anticheat::scan;
use crate::{http_client, AppConfig};

/// Сигнатура из блэклиста (`/api/anticheat/blacklist`).
#[derive(Debug, Clone, Deserialize)]
pub struct Signature {
    pub kind: String,
    pub pattern: String,
    #[serde(default)]
    pub severity: i32,
}

#[derive(Debug, Deserialize)]
#[serde(rename_all = "camelCase")]
struct InitResult {
    allowed: bool,
    #[serde(default)]
    reason: String,
    #[serde(default)]
    launch_token: String,
    #[serde(default)]
    nonce: String,
    #[serde(default)]
    challenge: String,
}

/// Исход handshake/init. Allowed/Blocked/UpdateRequired приходят от сервера;
/// Unavailable — сеть/сбой, означает FAIL-OPEN (запуск без enforcement, бэкенд
/// добьёт verified-гейтом на join).
pub enum InitOutcome {
    Allowed {
        token: String,
        nonce: String,
        challenge: String,
    },
    Blocked(String),
    UpdateRequired(String),
    Unavailable,
}

/// Тянет блэклист сигнатур (с auth). Err — недоступен (тогда скан пустой).
pub fn fetch_blacklist(config: &AppConfig, token: &str) -> Result<Vec<Signature>, String> {
    let client = http_client()?;
    let url = format!(
        "{}/api/anticheat/blacklist",
        config.api_url.trim_end_matches('/')
    );
    let response = client
        .get(url)
        .bearer_auth(token)
        .send()
        .map_err(|_| "blacklist fetch failed".to_string())?;
    if !response.status().is_success() {
        return Err("blacklist http error".to_string());
    }
    response
        .json::<Vec<Signature>>()
        .map_err(|_| "blacklist parse failed".to_string())
}

/// Выполняет handshake/init. Любой сетевой/парсинговый сбой → Unavailable (fail-open).
pub fn init(
    config: &AppConfig,
    token: &str,
    hwid_hash: &str,
    detections: &[scan::Detection],
) -> InitOutcome {
    let Ok(client) = http_client() else {
        return InitOutcome::Unavailable;
    };
    let url = format!(
        "{}/api/anticheat/handshake/init",
        config.api_url.trim_end_matches('/')
    );
    let body = serde_json::json!({
        "hwidHash": hwid_hash,
        "detections": detections,
    });
    let Ok(response) = client
        .post(url)
        .bearer_auth(token)
        // Серверный форс-апдейт: бэкенд отвечает 426, если версия ниже обязательной.
        .header("X-Launcher-Version", env!("CARGO_PKG_VERSION"))
        .json(&body)
        .send()
    else {
        return InitOutcome::Unavailable;
    };

    let status = response.status();
    // 426 = требуется обновление лаунчера: блокируем запуск с сообщением сервера.
    if status.as_u16() == 426 {
        let message = response
            .json::<serde_json::Value>()
            .ok()
            .and_then(|value| {
                value
                    .get("message")
                    .and_then(|m| m.as_str())
                    .map(String::from)
            })
            .unwrap_or_else(|| "Требуется обновление лаунчера.".to_string());
        return InitOutcome::UpdateRequired(message);
    }
    // 403 = заблокирован (allowed:false с причиной); success = разрешён.
    if status.as_u16() == 403 || status.is_success() {
        let Ok(result) = response.json::<InitResult>() else {
            return InitOutcome::Unavailable;
        };
        if !result.allowed {
            let reason = if result.reason.is_empty() {
                "Запуск заблокирован системой защиты.".to_string()
            } else {
                result.reason
            };
            return InitOutcome::Blocked(reason);
        }
        return InitOutcome::Allowed {
            token: result.launch_token,
            nonce: result.nonce,
            challenge: result.challenge,
        };
    }
    InitOutcome::Unavailable
}

/// Подтверждает handshake (с M3 это делает Java-агент в JVM, поэтому fallback/диагностика).
#[allow(dead_code)]
pub fn confirm(config: &AppConfig, launch_token: &str) -> Result<(), String> {
    if launch_token.is_empty() {
        return Err("Античит не инициализирован (нет launch-token).".to_string());
    }
    let client = http_client()?;
    let url = format!(
        "{}/api/anticheat/handshake/confirm",
        config.api_url.trim_end_matches('/')
    );
    let response = client
        .post(url)
        .json(&serde_json::json!({ "launchToken": launch_token }))
        .send()
        .map_err(|_| "Не удалось подтвердить защиту.".to_string())?;
    if !response.status().is_success() {
        return Err("Сервер отклонил подтверждение защиты.".to_string());
    }
    Ok(())
}
```

- [ ] **Step 2: Переписать `src/anticheat/mod.rs` как фасад**

Полностью заменить содержимое `mod.rs` на:

```rust
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
```

- [ ] **Step 3: Интегрировать в `launch_profile` (main.rs)**

Заменить начало `launch_profile` (строки 2125–2131):

```rust
    // Pre-launch античит: скан процессов, HWID, handshake, манифест целостности.
    // Блокирует запуск (Err) при бане/форс-апдейте. nonce связывает игровую сессию.
    let mut guard = anticheat::LaunchGuard::begin(config, token)?;
    let session = fetch_yggdrasil_session(config, token, guard.nonce())?;
```

Удалить строку получения манифеста (бывш. 2160 `let ac_manifest = ...`) — манифест теперь внутри guard.

Заменить authlib-блок (бывш. 2166–2176): SHA берётся из guard:

```rust
    if let Some(injector) = ensure_authlib_injector(config, guard.authlib_sha())? {
        let ygg_url = format!(
            "{}/api/v1/integrations/authlib/minecraft",
            config.api_url.trim_end_matches('/')
        );
        jvm_args.insert(
            0,
            format!("-javaagent:{}={}", injector.to_string_lossy(), ygg_url),
        );
    }
```

Заменить инжект-блок (введённый в Task 5) на вызов фасада:

```rust
    // было: let mut kick_file = None; if !guard.launch_token.is_empty() { inject::build... }
    guard.inject_into(&mut jvm_args, config)?;
```

Заменить блок чтения kick после выхода игры (введённый в Task 4):

```rust
    // было: if let Some(kick) = &kick_file { read_from + into_alert }
    if let Some(reason) = guard.finish() {
        return Err(reason.into_alert());
    }
```

Удалить более не нужную локальную `kick_file` и любые оставшиеся ссылки на `guard.launch_token`/`guard.nonce`/`guard.challenge` как на поля (теперь это методы / приватно). Проверить, что `guard` объявлен как `let mut guard` (нужно для `inject_into`).

- [ ] **Step 4: Сборка, тесты, clippy**

Run: `cd /home/liko/Разработка/Launcher/launcher-slint && cargo build 2>&1 | tail -30 && cargo test 2>&1 | tail -25 && cargo clippy --all-targets -- -D warnings 2>&1 | tail -20`
Expected: build OK, все тесты PASS, clippy чисто. Если компилятор укажет на оставшиеся обращения к старым полям `guard.*` — поправить на методы.

- [ ] **Step 5: Commit**

```bash
cd /home/liko/Разработка/Launcher && git add launcher-slint/src/anticheat/handshake.rs launcher-slint/src/anticheat/mod.rs launcher-slint/src/main.rs && git commit -m "$(cat <<'EOF'
refactor(launcher): фасад LaunchGuard + handshake.rs; явный fail-open

mod.rs теперь узкий фасад (begin/nonce/authlib_sha/inject_into/finish),
инкапсулирующий состояние сессии. handshake.rs: InitOutcome — единственное место
решения fail-open vs блок. launch_profile видит 3 метода вместо россыпи логики.

Co-Authored-By: Claude Opus 4.8 (1M context) <noreply@anthropic.com>
EOF
)"
```

---

## Task 7: Финальная верификация

**Files:** нет правок (проверочная задача). При обнаружении проблем — точечные фиксы и отдельный коммит.

- [ ] **Step 1: Полная проверка сборки/тестов/линта (release-профиль тоже)**

Run:
```bash
cd /home/liko/Разработка/Launcher/launcher-slint && \
  cargo build 2>&1 | tail -5 && \
  cargo build --release 2>&1 | tail -5 && \
  cargo test 2>&1 | tail -30 && \
  cargo clippy --all-targets -- -D warnings 2>&1 | tail -10
```
Expected: всё зелёное. Тесты: `artifacts::tests` (3), `anticheat::manifest::tests` (2), `anticheat::kick::tests` (3), `anticheat::inject::tests` (2), `anticheat::hwid::tests` (2, существующие), + существующие тесты main.rs — все PASS.

- [ ] **Step 2: Проверка отсутствия мёртвого кода и остаточных ссылок**

Run:
```bash
cd /home/liko/Разработка/Launcher/launcher-slint && \
  grep -n "fetch_anticheat_manifest\|ensure_agent_jar\|ensure_native_agent\|native_agent_target\|anticheat_kick_message\|ANTICHEAT_KICK_PREFIX\|pre_launch_guard\|AnticheatManifest\|fn verify_sha\|fn ensure_artifact\|fn download_and_verify" src/main.rs || echo "OK: остаточных ссылок в main.rs нет"
```
Expected: `OK: остаточных ссылок в main.rs нет`.

- [ ] **Step 3: Сверка эквивалентности инжекта по diff**

Просмотреть финальный diff `launch_profile` против `main` ветки и убедиться: authlib-инжект сохранён; набор `-Dac.*`/`-agentpath`/`-javaagent`/`-XX:+DisableAttachMechanism` тот же; единственное отличие — внутренний порядок (native перед agent) и инкапсуляция.

Run: `cd /home/liko/Разработка/Launcher && git diff main -- launcher-slint/src/main.rs | grep -E "^[+-].*(-Dac|agentpath|javaagent|DisableAttach|splice|insert)" | head -40`
Expected: видно удаление `insert(0..4)` и появление `splice`/`inject_into`; authlib `insert(0, ...-javaagent...)` сохранён.

- [ ] **Step 4: Ручной запуск игры (РУЧНОЙ шаг — выполняет пользователь)**

Инжект агентов нельзя проверить юнит-тестом — только живой JVM. Запустить лаунчер на модовом профиле (NeoForge) и убедиться: игра стартует, заходит на сервер (verified-сессия), при попытке инжекта чита срабатывает kick с уведомлением.

```bash
cd /home/liko/Разработка/Launcher && npm run dev:launcher
```
Ожидание: игра запускается и работает как до рефакторинга. Если падает с `Unable to access jarfile` или мгновенно выходит — регрессия инжекта, остановиться и разобрать.

- [ ] **Step 5: Финальный коммит верификации (если были фиксы) и сводка**

Если Step 1–4 выявили правки — закоммитить их. Иначе зафиксировать в сводке, что верификация пройдена. Ветка `refactor/anticheat-launcher` готова к слиянию через skill `finishing-a-development-branch`.

---

## Self-Review (выполнено при написании плана)

**Spec coverage:**
- ✅ Структура файлов (artifacts + 7 подмодулей anticheat) — Tasks 1–6.
- ✅ Границы доменов (artifacts общий, yggdrasil не трогаем) — Task 1 (DI клиента), Task 6 (authlib через guard.authlib_sha, yggdrasil-функции не тронуты).
- ✅ Улучшение 1 (единый объект) — Task 6 LaunchGuard + Task 5 константы флагов.
- ✅ Улучшение 2 (декларативный инжект) — Task 5.
- ✅ Улучшение 3 (общий загрузчик) — Task 1.
- ✅ Улучшение 4 (явный fail-open + типизированные ошибки) — Task 6 InitOutcome + Task 1 IntegrityError.
- ✅ Тестирование (manifest/inject/kick/artifacts) — юниты в Tasks 1,2,4,5.
- ✅ Верификация эквивалентности + ручной запуск — Task 7.
- ✅ fail-open сохранён — явно в InitOutcome::Unavailable и artifacts Ok(None).

**Placeholder scan:** код приведён полностью во всех Create-шагах; правки main.rs указаны с конкретными строками и финальным кодом. Нет TBD/«добавить обработку ошибок».

**Type consistency:** `IntegrityManifest`, `InjectionPlan{args,kick_file}`, `InitOutcome`, `KickReason`, `IntegrityError::Tampered`, `LaunchGuard` методы — имена согласованы между задачами и совпадают со spec. `inject::build(token, challenge, manifest, config)` — сигнатура одинакова в Task 5 (Produces) и Task 6 (Consumes/inject_into).
