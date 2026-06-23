# Discord Rich Presence Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Показывать в Discord статус игрока (Rich Presence) с четырьмя стадиями — в лаунчере, выбирает сборку, загружает файлы, играет на сервере Project: Minecraft — опционально и без влияния на работу лаунчера при отсутствии Discord.

**Architecture:** Новый модуль `src/discord_rpc.rs` с фоновым потоком-актором, владеющим IPC-соединением `discord-rich-presence`. Главный поток шлёт команды через `mpsc`-канал, глобальный `Sender` живёт в `OnceLock` (по образцу `UPDATE_SHARED`). Чистая функция `presence_to_activity_fields` маппит стадию в поля activity и покрывается юнит-тестами; всё остальное — интеграция, проверяется сборкой и ручным прогоном.

**Tech Stack:** Rust, Slint 1.16, крейт `discord-rich-presence` 1.1, `std::sync::mpsc`, `std::thread`.

## Global Constraints

- Лаунчер обязан работать идентично без Discord: на путях RPC **нет** `unwrap`/`expect`, все ошибки IPC проглатываются.
- Версия крейта: `discord-rich-presence = "1.1"`.
- Client ID берётся через `option_env!("DISCORD_CLIENT_ID")` с fallback на константу `DEFAULT_DISCORD_CLIENT_ID` (по образцу `LAUNCHER_DEFAULT_API_URL` в `src/main.rs:431`). При Client ID `"0"` или пустом — актор не запускается, фича выключена.
- Тогл настройки `discord_rpc_enabled` по умолчанию `true`, паттерн полностью повторяет `use_discrete_gpu`.
- Большая иконка везде — ключ `logo`. Маленькие иконки: `idle`, `download`, `playing`.
- Тексты стадий (RU, дословно):
  - Idle → details `В главном меню`, без state.
  - Browsing → details `Выбирает сборку`, state `<ник>`, small `idle`.
  - Downloading → details `Загружает сборку`, state `<имя сборки>`, small `download`.
  - Playing → details `Играет на Project: Minecraft`, state `<имя сборки>`, small `playing`, timestamp start = время запуска игры.
- Запуск `cargo` в этом проекте — через Docker нельзя (нужен GUI-кодоген Slint); сборка лаунчера идёт локальным `cargo` (`npm run dev:launcher` / `cargo build -p launcher-slint`). Go-докер тут ни при чём.

---

## File Structure

- **Create** `launcher-slint/src/discord_rpc.rs` — весь модуль RPC: типы `Presence`/`RpcCommand`/`ActivityFields`, чистая функция `presence_to_activity_fields`, актор-поток, глобальный хэндл и хелперы `rpc_init`/`rpc_set`/`rpc_set_enabled`/`rpc_shutdown`.
- **Modify** `launcher-slint/Cargo.toml` — добавить зависимость.
- **Modify** `launcher-slint/build.rs` — `rerun-if-env-changed=DISCORD_CLIENT_ID`.
- **Modify** `launcher-slint/src/main.rs` — `mod discord_rpc;`, поле настройки, точки интеграции, обработчик тогла.
- **Modify** `launcher-slint/ui/app.slint` — свойство, колбэк, тогл-секция, правки координат карточки настроек.

Все пути ниже — относительно `launcher-slint/`. Рабочая директория для всех `cargo`-команд: `launcher-slint/`.

---

### Task 1: Зависимость, env-проброс и константа Client ID

**Files:**
- Modify: `Cargo.toml`
- Modify: `build.rs`

**Interfaces:**
- Produces: для следующих задач становится доступен крейт `discord-rich-presence` и env-переменная `DISCORD_CLIENT_ID` при сборке.

- [ ] **Step 1: Добавить зависимость в `Cargo.toml`**

В секцию `[dependencies]` (после строки `directories = "6"`) добавить:

```toml
discord-rich-presence = "1.1"
```

- [ ] **Step 2: Пробросить env в `build.rs`**

В `build.rs` добавить строку перед вызовом `slint_build::compile`:

```rust
fn main() {
    println!("cargo:rerun-if-env-changed=LAUNCHER_DEFAULT_API_URL");
    println!("cargo:rerun-if-env-changed=DISCORD_CLIENT_ID");
    slint_build::compile("ui/app.slint").expect("failed to compile Slint UI");
}
```

- [ ] **Step 3: Проверить, что зависимость резолвится**

Run: `cargo fetch` (в `launcher-slint/`)
Expected: завершается без ошибок, в выводе появляется `discord-rich-presence v1.1.x` (или скачивание оного).

- [ ] **Step 4: Commit**

```bash
git add launcher-slint/Cargo.toml launcher-slint/Cargo.lock launcher-slint/build.rs
git commit -m "build(launcher): зависимость discord-rich-presence + env DISCORD_CLIENT_ID"
```

---

### Task 2: Модуль discord_rpc — типы и чистый маппинг (TDD)

**Files:**
- Create: `src/discord_rpc.rs`
- Modify: `src/main.rs` (добавить `mod discord_rpc;`)

**Interfaces:**
- Produces:
  - `pub enum Presence { Idle, Browsing { nick: String }, Downloading { profile: String }, Playing { profile: String, started_at: i64 } }`
  - `pub struct ActivityFields { pub details: &'static str, pub state: Option<String>, pub small_image: Option<&'static str>, pub timestamp_start: Option<i64> }`
  - `pub fn presence_to_activity_fields(p: &Presence) -> ActivityFields`
  - `pub const LARGE_IMAGE: &str = "logo";`
- Consumes: ничего (первая задача модуля).

- [ ] **Step 1: Создать `src/discord_rpc.rs` с типами и чистой функцией + тестами**

Создать файл `src/discord_rpc.rs` со следующим содержимым:

```rust
//! Discord Rich Presence: фоновый актор + чистый маппинг стадий в activity-поля.
//! Полностью опционален: при отсутствии Discord/Client ID лаунчер работает как обычно.

/// Стадия лаунчера, отражаемая в Discord.
#[derive(Clone, Debug, PartialEq)]
pub enum Presence {
    /// Лаунчер открыт, игрок не в сессии.
    Idle,
    /// Залогинен, смотрит профили.
    Browsing { nick: String },
    /// Идёт скачивание/подготовка сборки.
    Downloading { profile: String },
    /// Запущен Minecraft. `started_at` — Unix-секунды старта игры.
    Playing { profile: String, started_at: i64 },
}

/// Большая иконка presence — лого проекта (ключ арт-ассета в Developer Portal).
pub const LARGE_IMAGE: &str = "logo";

/// Поля, из которых актор собирает `activity::Activity`.
#[derive(Clone, Debug, PartialEq)]
pub struct ActivityFields {
    pub details: &'static str,
    pub state: Option<String>,
    pub small_image: Option<&'static str>,
    pub timestamp_start: Option<i64>,
}

/// Чистый маппинг стадии в поля activity. Тестируется без реального IPC.
pub fn presence_to_activity_fields(p: &Presence) -> ActivityFields {
    match p {
        Presence::Idle => ActivityFields {
            details: "В главном меню",
            state: None,
            small_image: None,
            timestamp_start: None,
        },
        Presence::Browsing { nick } => ActivityFields {
            details: "Выбирает сборку",
            state: Some(nick.clone()),
            small_image: Some("idle"),
            timestamp_start: None,
        },
        Presence::Downloading { profile } => ActivityFields {
            details: "Загружает сборку",
            state: Some(profile.clone()),
            small_image: Some("download"),
            timestamp_start: None,
        },
        Presence::Playing { profile, started_at } => ActivityFields {
            details: "Играет на Project: Minecraft",
            state: Some(profile.clone()),
            small_image: Some("playing"),
            timestamp_start: Some(*started_at),
        },
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn idle_has_no_state_or_timestamp() {
        let f = presence_to_activity_fields(&Presence::Idle);
        assert_eq!(f.details, "В главном меню");
        assert_eq!(f.state, None);
        assert_eq!(f.small_image, None);
        assert_eq!(f.timestamp_start, None);
    }

    #[test]
    fn browsing_shows_nick() {
        let f = presence_to_activity_fields(&Presence::Browsing { nick: "Steve".into() });
        assert_eq!(f.details, "Выбирает сборку");
        assert_eq!(f.state, Some("Steve".to_string()));
        assert_eq!(f.small_image, Some("idle"));
    }

    #[test]
    fn downloading_shows_profile() {
        let f = presence_to_activity_fields(&Presence::Downloading { profile: "Pixelmon".into() });
        assert_eq!(f.details, "Загружает сборку");
        assert_eq!(f.state, Some("Pixelmon".to_string()));
        assert_eq!(f.small_image, Some("download"));
    }

    #[test]
    fn playing_carries_timestamp() {
        let f = presence_to_activity_fields(&Presence::Playing {
            profile: "Pixelmon".into(),
            started_at: 1_700_000_000,
        });
        assert_eq!(f.details, "Играет на Project: Minecraft");
        assert_eq!(f.state, Some("Pixelmon".to_string()));
        assert_eq!(f.small_image, Some("playing"));
        assert_eq!(f.timestamp_start, Some(1_700_000_000));
    }
}
```

- [ ] **Step 2: Подключить модуль в `src/main.rs`**

В начало `src/main.rs`, рядом с другими `mod`-объявлениями (там уже есть `mod gpu;`, `mod updater;` — найди их в начале файла), добавить:

```rust
mod discord_rpc;
```

- [ ] **Step 3: Запустить тесты — убедиться, что проходят**

Run: `cargo test -p launcher-slint discord_rpc`
Expected: 4 теста PASS (`idle_has_no_state_or_timestamp`, `browsing_shows_nick`, `downloading_shows_profile`, `playing_carries_timestamp`).

- [ ] **Step 4: Commit**

```bash
git add launcher-slint/src/discord_rpc.rs launcher-slint/src/main.rs
git commit -m "feat(launcher): discord_rpc — типы Presence и маппинг activity (TDD)"
```

---

### Task 3: Актор-поток и глобальный хэндл

**Files:**
- Modify: `src/discord_rpc.rs`

**Interfaces:**
- Consumes: `Presence`, `ActivityFields`, `presence_to_activity_fields`, `LARGE_IMAGE` из Task 2.
- Produces:
  - `pub fn rpc_init(client_id: &str)` — один раз создаёт актор-поток; no-op если `client_id` пустой или `"0"`, либо если уже инициализирован.
  - `pub fn rpc_set(p: Presence)` — отправить стадию (no-op без инициализации).
  - `pub fn rpc_set_enabled(on: bool)` — вкл/выкл presence.
  - `pub const DEFAULT_DISCORD_CLIENT_ID: &str = "0";`

- [ ] **Step 1: Добавить актор, команды и хелперы в `src/discord_rpc.rs`**

В верх файла (под docstring) добавить импорты:

```rust
use std::sync::mpsc::{self, Receiver, Sender};
use std::sync::OnceLock;
use std::time::Duration;

use discord_rich_presence::activity::{Activity, Assets, Timestamps};
use discord_rich_presence::{DiscordIpc, DiscordIpcClient};
```

В конец файла (перед `#[cfg(test)] mod tests`) добавить:

```rust
/// Плейсхолдер Client ID. Реальный ID подставляется через env `DISCORD_CLIENT_ID`
/// при сборке (см. main.rs). При значении "0"/пустом RPC не запускается.
pub const DEFAULT_DISCORD_CLIENT_ID: &str = "0";

/// Команда актору.
enum RpcCommand {
    Set(Presence),
    SetEnabled(bool),
}

static RPC: OnceLock<Sender<RpcCommand>> = OnceLock::new();

/// Создаёт фоновый актор-поток. Вызывать один раз из `main`. No-op, если
/// Client ID не задан (placeholder) или актор уже запущен.
pub fn rpc_init(client_id: &str) {
    let client_id = client_id.trim();
    if client_id.is_empty() || client_id == "0" {
        return;
    }
    if RPC.get().is_some() {
        return;
    }
    let (tx, rx) = mpsc::channel::<RpcCommand>();
    if RPC.set(tx).is_err() {
        return; // гонка инициализации — другой вызов уже выставил Sender.
    }
    let client_id = client_id.to_string();
    std::thread::spawn(move || actor_loop(client_id, rx));
}

/// Отправить стадию актору. Никогда не паникует и не блокирует надолго.
pub fn rpc_set(p: Presence) {
    if let Some(tx) = RPC.get() {
        let _ = tx.send(RpcCommand::Set(p));
    }
}

/// Включить/выключить presence (тогл настроек).
pub fn rpc_set_enabled(on: bool) {
    if let Some(tx) = RPC.get() {
        let _ = tx.send(RpcCommand::SetEnabled(on));
    }
}

/// Интервал попытки переподключения, когда Discord недоступен.
const RECONNECT_INTERVAL: Duration = Duration::from_secs(15);

/// Цикл актора: держит соединение, применяет стадии, переподключается.
/// Все ошибки IPC проглатываются — сбой RPC не влияет на лаунчер.
fn actor_loop(client_id: String, rx: Receiver<RpcCommand>) {
    let mut client = match DiscordIpcClient::new(&client_id) {
        Ok(c) => c,
        Err(_) => return, // невозможно создать клиент — тихо выходим.
    };
    let mut connected = client.connect().is_ok();
    let mut enabled = true;
    let mut last: Option<Presence> = None;

    loop {
        // Ждём команду; по таймауту пробуем переподключиться и переприменить.
        let cmd = rx.recv_timeout(RECONNECT_INTERVAL);
        match cmd {
            Ok(RpcCommand::Set(p)) => {
                last = Some(p);
            }
            Ok(RpcCommand::SetEnabled(on)) => {
                enabled = on;
                if !enabled {
                    // Discord закрыт → clear безуспешен, не страшно.
                    let _ = client.clear_activity();
                    continue;
                }
            }
            Err(mpsc::RecvTimeoutError::Timeout) => {
                // Периодический тик: ниже попробуем (пере)подключиться и применить.
            }
            Err(mpsc::RecvTimeoutError::Disconnected) => {
                let _ = client.close();
                return; // Sender уничтожен (выход приложения) — завершаем поток.
            }
        }

        // Гарантируем соединение.
        if !connected {
            connected = client.connect().is_ok();
        }
        if !connected {
            continue; // Discord не запущен — ждём следующий тик/команду.
        }

        // Применяем текущую стадию (если включено и есть что показывать).
        if enabled {
            if let Some(p) = &last {
                if !apply_presence(&mut client, p) {
                    // Запись провалилась — соединение протухло, сбросим флаг.
                    connected = false;
                }
            }
        }
    }
}

/// Собирает Activity из стадии и пишет в Discord. Возвращает false при ошибке IPC.
fn apply_presence(client: &mut DiscordIpcClient, p: &Presence) -> bool {
    let fields = presence_to_activity_fields(p);

    let mut assets = Assets::new().large_image(LARGE_IMAGE);
    if let Some(small) = fields.small_image {
        assets = assets.small_image(small);
    }

    let mut activity = Activity::new().details(fields.details).assets(assets);
    if let Some(state) = &fields.state {
        activity = activity.state(state.as_str());
    }
    if let Some(start) = fields.timestamp_start {
        activity = activity.timestamps(Timestamps::new().start(start));
    }

    client.set_activity(activity).is_ok()
}
```

Примечание для исполнителя: `Activity<'a>` заимствует строки, поэтому `fields`,
`assets`, `activity` живут в одной области видимости до вызова `set_activity` —
именно так и написано выше, не выноси их за пределы функции.

- [ ] **Step 2: Собрать модуль — проверить компиляцию API крейта**

Run: `cargo build -p launcher-slint`
Expected: компиляция успешна. Если компилятор ругается на сигнатуры `Activity`/`Assets`/`Timestamps` — сверь с `https://docs.rs/discord-rich-presence/1.1.0/` (методы `state`/`details` принимают `Into<Cow<str>>`, `Timestamps::start(i64)`).

- [ ] **Step 3: Прогнать существующие тесты — не сломались**

Run: `cargo test -p launcher-slint discord_rpc`
Expected: 4 теста из Task 2 по-прежнему PASS.

- [ ] **Step 4: Commit**

```bash
git add launcher-slint/src/discord_rpc.rs
git commit -m "feat(launcher): discord_rpc — актор-поток с переподключением и хелперы"
```

---

### Task 4: Настройка discord_rpc_enabled

**Files:**
- Modify: `src/main.rs` (struct `LauncherSettings` ~`:370`, `impl Default` ~`:380`, default-fn рядом с `default_use_discrete_gpu` ~`:2692`)

**Interfaces:**
- Produces: поле `LauncherSettings.discord_rpc_enabled: bool` (default `true`), функция `default_discord_rpc_enabled() -> bool`.

- [ ] **Step 1: Написать тест на serde-дефолт**

В `src/main.rs` найди блок `#[cfg(test)] mod tests` (если его нет в этом файле — добавь в конец файла новый блок). Добавь тест:

```rust
#[test]
fn discord_rpc_enabled_defaults_true_when_absent() {
    // Старый settings.json без поля discord_rpc_enabled должен десериализоваться
    // с дефолтом true (фича включена по умолчанию).
    let json = r#"{"memoryGb":4,"memoryAuto":true,"useDiscreteGpu":true}"#;
    let s: LauncherSettings = serde_json::from_str(json).unwrap();
    assert!(s.discord_rpc_enabled);
}
```

- [ ] **Step 2: Запустить тест — убедиться, что падает компиляцией/ассертом**

Run: `cargo test -p launcher-slint discord_rpc_enabled_defaults_true_when_absent`
Expected: FAIL — поле `discord_rpc_enabled` ещё не существует (ошибка компиляции "no field").

- [ ] **Step 3: Добавить поле и дефолт**

В struct `LauncherSettings` (после строки с `use_discrete_gpu: bool,`) добавить:

```rust
    #[serde(default = "default_discord_rpc_enabled")]
    discord_rpc_enabled: bool,
```

В `impl Default for LauncherSettings` (после `use_discrete_gpu: true,`) добавить:

```rust
            discord_rpc_enabled: true,
```

Рядом с `fn default_use_discrete_gpu() -> bool { ... }` (около `src/main.rs:2692`) добавить:

```rust
fn default_discord_rpc_enabled() -> bool {
    true
}
```

- [ ] **Step 4: Запустить тест — проходит**

Run: `cargo test -p launcher-slint discord_rpc_enabled_defaults_true_when_absent`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add launcher-slint/src/main.rs
git commit -m "feat(launcher): настройка discord_rpc_enabled (default on)"
```

---

### Task 5: UI — свойство, колбэк и тогл-секция в настройках

**Files:**
- Modify: `ui/app.slint` (свойства ~`:417`, callbacks ~`:442`, карточка настроек `settings-card` ~`:1223`, секция GPU ~`:1478`, секция «Папка установки» ~`:1545`)

**Interfaces:**
- Consumes: ничего из Rust на этом шаге (привязка в Task 6).
- Produces: свойство `discord-rpc-enabled: bool`, колбэк `discord-rpc-requested(bool)`.

Контекст по верстке (важно): карточка настроек использует **абсолютное позиционирование** (см. гочу в CLAUDE.md). Сейчас:
- `settings-card.height: root.discrete-gpu-available ? 574px : 470px;` (строка ~1226)
- секция GPU: `y: 288px`, `height: 88px` (видна только при `discrete-gpu-available`)
- секция «Папка установки»: `y: root.discrete-gpu-available ? 392px : 288px;`, `height: 150px` (строка ~1545)

Добавляем секцию Discord RPC **между** GPU и папкой установки логически, но физически — фиксируем её координаты и сдвигаем папку установки и высоту карточки на +104px (88px секция + 16px зазор). Discord-секция видна всегда (не зависит от GPU), поэтому её `y` тоже зависит от `discrete-gpu-available`.

- [ ] **Step 1: Добавить свойство и колбэк**

В блок свойств `AppWindow` (после `in property <bool> use-discrete-gpu;`, строка ~417) добавить:

```slint
    in property <bool> discord-rpc-enabled: true;
```

В блок колбэков (рядом с `callback discrete-gpu-requested(bool);`, строка ~442) добавить:

```slint
    callback discord-rpc-requested(bool);
```

- [ ] **Step 2: Поднять высоту карточки настроек**

Заменить строку (~1226):

```slint
            height: root.discrete-gpu-available ? 574px : 470px;
```

на:

```slint
            // +104px на секцию Discord Rich Presence (всегда видна).
            height: root.discrete-gpu-available ? 678px : 574px;
```

- [ ] **Step 3: Сдвинуть секцию «Папка установки» вниз**

Заменить строку (~1545) у секции «Папка установки»:

```slint
                y: root.discrete-gpu-available ? 392px : 288px;
```

на:

```slint
                y: root.discrete-gpu-available ? 496px : 392px;
```

- [ ] **Step 4: Добавить секцию-тогл Discord RPC**

Сразу **после** закрывающей `}` секции дискретной видеокарты (она заканчивается перед секцией «Папка установки», около строки 1543) вставить новый блок:

```slint
            // Секция Discord Rich Presence (всегда видна).
            Rectangle {
                x: 28px;
                y: root.discrete-gpu-available ? 392px : 288px;
                width: parent.width - 56px;
                height: 88px;
                border-radius: 14px;
                background: #131313;
                border-color: #ffffff12;
                border-width: 1px;

                Text {
                    text: "Discord Rich Presence";
                    x: 22px;
                    y: 18px;
                    width: parent.width - 130px;
                    height: 24px;
                    color: #e0e2e5;
                    font-size: 18px;
                    font-weight: 800;
                    overflow: elide;
                }

                Text {
                    text: "Показывать статус игры в профиле Discord";
                    x: 22px;
                    y: 46px;
                    width: parent.width - 130px;
                    height: 20px;
                    color: #8c8f95;
                    font-size: 13px;
                    font-weight: 700;
                    overflow: elide;
                }

                // Переключатель ВКЛ/ВЫКЛ.
                Rectangle {
                    x: parent.width - 92px;
                    y: (parent.height - 34px) / 2;
                    width: 70px;
                    height: 34px;
                    border-radius: 17px;
                    background: root.discord-rpc-enabled ? #2f6f3a : #1a1a1a;
                    border-color: root.discord-rpc-enabled ? #4ea35e88 : #ffffff1a;
                    border-width: 1px;
                    animate background, border-color { duration: 200ms; easing: ease-out-quad; }

                    Text {
                        text: root.discord-rpc-enabled ? "ВКЛ" : "ВЫКЛ";
                        color: root.discord-rpc-enabled ? #eafaee : #989ca2;
                        font-size: 12px;
                        font-weight: 900;
                        letter-spacing: 0.6px;
                        horizontal-alignment: center;
                        vertical-alignment: center;
                    }

                    discord-rpc-touch := TouchArea {
                        enabled: root.settings-visible;
                        mouse-cursor: pointer;
                        clicked => { root.discord-rpc-requested(!root.discord-rpc-enabled); }
                    }
                }
            }
```

- [ ] **Step 5: Собрать — проверить кодоген Slint**

Run: `cargo build -p launcher-slint`
Expected: компиляция успешна (Slint-кодоген видит новое свойство/колбэк; Rust пока не привязан — это нормально, свойства/колбэки без обработчика допустимы).

- [ ] **Step 6: Commit**

```bash
git add launcher-slint/ui/app.slint
git commit -m "feat(launcher): тогл Discord Rich Presence в настройках (UI)"
```

---

### Task 6: Интеграция — инициализация, точки стадий, обработчик тогла

**Files:**
- Modify: `src/main.rs` (`main()` ~`:431`–`:470`, `apply_session` ~`:870`, `register_logout_handler` ~`:540`, `register_play_handler` ~`:698`, `launch_profile` ~`:2197`, `register_settings_handler` ~`:622`, `apply_launcher_settings` ~`:2508`)

**Interfaces:**
- Consumes: `discord_rpc::{rpc_init, rpc_set, rpc_set_enabled, Presence, DEFAULT_DISCORD_CLIENT_ID}` из Task 2/3; поле `discord_rpc_enabled` из Task 4; свойство/колбэк из Task 5.

- [ ] **Step 1: Инициализация RPC в `main()`**

В `main()`, сразу после блока `let config = AppConfig { ... };` (около `src/main.rs:436`, до `let app = AppWindow::new()?;`), добавить:

```rust
    // Discord Rich Presence (опционально). Client ID — из env при сборке или
    // константы-плейсхолдера; при "0" rpc_init — no-op.
    let discord_client_id = option_env!("DISCORD_CLIENT_ID")
        .unwrap_or(discord_rpc::DEFAULT_DISCORD_CLIENT_ID);
    discord_rpc::rpc_init(discord_client_id);
```

Затем, **после** строки `apply_launcher_settings(&app, &load_settings().unwrap_or_default());` (около `src/main.rs:459`), добавить применение тогла и стартовой стадии:

```rust
    {
        let settings = load_settings().unwrap_or_default();
        discord_rpc::rpc_set_enabled(settings.discord_rpc_enabled);
        discord_rpc::rpc_set(discord_rpc::Presence::Idle);
    }
```

- [ ] **Step 2: Browsing при входе — в `apply_session`**

В `apply_session` (около `src/main.rs:873`), внутри блока `if let Ok(mut state) = state.lock() { ... }` после `state.user = Some(session.user.clone());` уже есть доступ к нику. В **конце** функции `apply_session` (после `apply_install_folder_label(app, state);`, перед закрывающей `}` функции — найди по контексту) добавить:

```rust
    discord_rpc::rpc_set(discord_rpc::Presence::Browsing {
        nick: session.user.login.clone(),
    });
```

- [ ] **Step 3: Idle при логауте — в `register_logout_handler`**

В `register_logout_handler` (около `src/main.rs:545`), внутри `on_logout_requested`, после `let _ = delete_token();` добавить:

```rust
        discord_rpc::rpc_set(discord_rpc::Presence::Idle);
```

- [ ] **Step 4: Downloading при старте подготовки — в `register_play_handler`**

В `register_play_handler` (около `src/main.rs:730`), внутри `if let Some(app) = play_app.upgrade() { ... }` блока, где выставляется `app.set_download_phase("Получаем профиль".into());` — сразу после этого `if`-блока (перед `let app_weak = play_app.clone();`) добавить:

```rust
        discord_rpc::rpc_set(discord_rpc::Presence::Downloading {
            profile: profile.name.clone(),
        });
```

- [ ] **Step 5: Browsing после завершения игры — в результат-замыкании `register_play_handler`**

В том же `register_play_handler`, в замыкании `invoke_from_event_loop` (где обрабатывается `result` от `sync_and_launch`, около `src/main.rs:746`), в **самом начале** замыкания (после `if let Some(app) = app_weak.upgrade() {`) вернуть presence к выбору сборки. Ник возьмём из `user`, захваченного в замыкание. Для этого сначала склонировать ник перед `thread::spawn`:

Найди строку `let app_weak = play_app.clone();` (перед `thread::spawn`) и добавь рядом:

```rust
        let nick_for_rpc = user.login.clone();
```

Затем `nick_for_rpc` нужно переместить в поток. В `thread::spawn(move || { ... })` он уже захватится по `move`. Внутри замыкания `invoke_from_event_loop(move || { ... })` — добавить в начало (после `if let Some(app) = app.upgrade() {` ... здесь переменная называется `app`, сверься с фактическим кодом):

```rust
                discord_rpc::rpc_set(discord_rpc::Presence::Browsing {
                    nick: nick_for_rpc.clone(),
                });
```

Примечание: `nick_for_rpc` захватывается во внешний `move`-замыкание потока, затем во внутренний `move`-замыкание event loop — поэтому `.clone()` при использовании. Если borrow-checker ругается, склонируй `nick_for_rpc` повторно перед `invoke_from_event_loop`.

- [ ] **Step 6: Playing при запуске игры — в `launch_profile`**

В `launch_profile`, рядом со `spawn` процесса (около `src/main.rs:2197`), **до** `let mut child = process.spawn()...` добавить захват времени старта и имени:

```rust
    let started_at = std::time::SystemTime::now()
        .duration_since(std::time::UNIX_EPOCH)
        .map(|d| d.as_secs() as i64)
        .unwrap_or(0);
```

Затем сразу **после** `post_game_started(app);` (она вызывается после успешного `spawn`, около `src/main.rs:2210`) добавить:

```rust
    discord_rpc::rpc_set(discord_rpc::Presence::Playing {
        profile: manifest.profile.name.clone(),
        started_at,
    });
```

(Возврат к Browsing после `child.wait()` уже покрыт Step 5 — он срабатывает в обработчике результата `register_play_handler` для обоих исходов Ok/Err.)

- [ ] **Step 7: Обработчик тогла — в `register_settings_handler`**

В `register_settings_handler` (около `src/main.rs:622`), сразу **после** блока `app.on_discrete_gpu_requested(move |enabled| { ... });` добавить аналогичный обработчик:

```rust
    let discord_app = app.as_weak();
    app.on_discord_rpc_requested(move |enabled| {
        if let Some(app) = discord_app.upgrade() {
            let mut settings = load_settings().unwrap_or_default();
            settings.discord_rpc_enabled = enabled;
            match save_settings(&settings) {
                Ok(()) => {
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
```

- [ ] **Step 8: Прокинуть значение в UI — в `apply_launcher_settings`**

В `apply_launcher_settings` (около `src/main.rs:2508`), в конце функции (после `app.set_use_discrete_gpu(settings.use_discrete_gpu);`) добавить:

```rust
    app.set_discord_rpc_enabled(settings.discord_rpc_enabled);
```

- [ ] **Step 9: Собрать и прогнать тесты**

Run: `cargo build -p launcher-slint && cargo test -p launcher-slint`
Expected: сборка успешна, все тесты PASS (4 из discord_rpc + serde-дефолт из Task 4).

- [ ] **Step 10: Commit**

```bash
git add launcher-slint/src/main.rs
git commit -m "feat(launcher): интеграция Discord RPC во все стадии лаунчера"
```

---

### Task 7: Ручная проверка и финальная верификация

**Files:** нет правок (только проверка).

- [ ] **Step 1: Полная сборка и проверки**

Run: `cargo build -p launcher-slint && cargo test -p launcher-slint`
Expected: успешно, все тесты PASS.

- [ ] **Step 2: Ручной прогон с реальным Client ID**

При установленном Discord-клиенте:

```bash
DISCORD_CLIENT_ID=<реальный_client_id> cargo run -p launcher-slint
```

Проверить последовательно (Discord → профиль пользователя):
- До логина — статус «В главном меню».
- После логина — «Выбирает сборку» + ник.
- Нажать «Играть» — «Загружает сборку» + имя профиля.
- Игра запущена — «Играет на Project: Minecraft» + имя сборки + тикающий таймер.
- Закрыть игру — возврат к «Выбирает сборку».
- В настройках выключить тогл — presence исчезает из Discord; включить — возвращается.
- Запустить **без** `DISCORD_CLIENT_ID` (placeholder "0") — лаунчер работает штатно, presence отсутствует, ошибок нет.

- [ ] **Step 3: Зафиксировать Client ID в коде (когда владелец предоставит)**

Заменить в `src/discord_rpc.rs` значение `DEFAULT_DISCORD_CLIENT_ID` с `"0"` на реальный Client ID (если решено вшивать в код, а не только через env). Пересобрать, повторить Step 2 без env-переменной.

```bash
git add launcher-slint/src/discord_rpc.rs
git commit -m "chore(launcher): вшить реальный Discord Client ID"
```

- [ ] **Step 4: Напоминание про арт-ассеты Discord**

Убедиться, что в Discord Developer Portal → приложение → Rich Presence → Art Assets загружены изображения с ключами: `logo`, `idle`, `download`, `playing`. Без них presence покажется без иконок (текст всё равно работает).

---

## Self-Review

**Spec coverage:**
- Модуль `discord_rpc.rs` + актор → Task 2, 3. ✔
- 4 стадии и их тексты/ассеты → константы в Task 2, маппинг покрыт тестами. ✔
- Таймер elapsed на Playing → Task 2 (`timestamp_start`), Task 6 Step 6 (`started_at`). ✔
- Ник на Browsing → Task 2, Task 6 Step 2/5. ✔
- Client ID через env-override + константа → Task 1, Task 3 (`DEFAULT_DISCORD_CLIENT_ID`), Task 6 Step 1. ✔
- Тогл настройки `discord_rpc_enabled` default true → Task 4 (модель), Task 5 (UI), Task 6 Step 7/8 (обработчик). ✔
- Точки интеграции (старт/логин/логаут/загрузка/игра/закрытие) → Task 6. ✔
- Обработка ошибок без unwrap, фича опциональна → актор в Task 3 (`is_ok()`/проглатывание), `rpc_init` no-op при "0". ✔
- Тестирование: юнит на маппинг + сборка + ручной прогон → Task 2, 7. ✔
- YAGNI (без party/join, без per-профиль иконок, без процентов) → не реализуется. ✔

**Placeholder scan:** плейсхолдер только намеренный — `DEFAULT_DISCORD_CLIENT_ID = "0"` (описан в Global Constraints и Task 7 Step 3). Других TBD/TODO нет.

**Type consistency:** `Presence`/`ActivityFields`/`presence_to_activity_fields`/`LARGE_IMAGE` (Task 2) используются в Task 3 и Task 6 под теми же именами; `rpc_init`/`rpc_set`/`rpc_set_enabled` (Task 3) — те же в Task 6; поле `discord_rpc_enabled` (Task 4) — то же в Task 6; свойство `discord-rpc-enabled` и колбэк `discord-rpc-requested` (Task 5) — те же `set_discord_rpc_enabled`/`on_discord_rpc_requested` в Task 6. ✔
