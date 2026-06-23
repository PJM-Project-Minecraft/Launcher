# Рефакторинг античита в десктоп-лаунчере (launcher-slint)

**Дата:** 2026-06-23
**Компонент:** `launcher-slint/` (Rust + Slint), модуль `src/anticheat/` и связанная логика в `src/main.rs`
**Тип:** рефакторинг с переносом логики в модуль + точечные улучшения (поведение enforcement сохраняется)

## Контекст и проблема

Сам модуль `src/anticheat/` компактный и чистый (handshake, HWID, скан процессов — 417 строк),
но значительная часть античит-логики живёт в `main.rs` — монолите на 3614 строк (~150 функций),
где находится всё, кроме явно выделенных модулей (`anticheat`, `discord_rpc`, `gpu`, `updater`).

Размазано по `main.rs`:
- структуры манифеста целостности + `fetch_anticheat_manifest`, `sha_opt` (1328–1381);
- верифицированный загрузчик `verify_sha` / `download_and_verify` / `ensure_artifact` (1385–1464);
- управление агентами `ensure_agent_jar` / `ensure_native_agent` / `native_agent_target` (1484–1532);
- инжект агентов в JVM внутри `launch_profile` (2178–2214): хрупкие `jvm_args.insert(0/1/2/3/4,…)`,
  где порядок индексов критичен (native вставляет на 0–2, затем agent сдвигает их вставкой на 0–4),
  вперемешку с управлением флаг-файлами по хардкоженным строкам;
- обработка kick: `ANTICHEAT_KICK_PREFIX` (36), `anticheat_kick_message` (2302), чтение kick-файла (2286–2296).

## Цели

Два драйвера (по итогам брейншторма):

1. **Логика размазана по `main.rs`** — собрать всё античит-связанное в модуль с чистыми границами.
2. **Надёжность / архитектура enforcement** — разбросанное состояние сессии (token/nonce/challenge/kickfile),
   хрупкая последовательность запуска, неявный fail-open.

Граница: **рефакторинг с переносом в модуль + точечные улучшения**, **без** глубокого редизайна enforcement.
Политика **fail-open сохраняется** (при недоступном бэкенде игрок не блокируется — enforcement приходит позже
через verified-гейт на join). Менять fail-open на fail-closed в рамках этой задачи **нельзя**: это напрямую
повлияло бы на живых игроков.

### Точечные улучшения в scope (все четыре)

1. Единый объект античит-сессии вместо разбросанных переменных и хардкоженных строк.
2. Декларативная сборка JVM-инжекта вместо хрупких `insert` по индексам.
3. Общий верифицированный загрузчик артефактов (agent.jar / native / authlib) с чистой границей.
4. Явная fail-open политика + типизированные ошибки вместо строковых.

## Архитектура

### Целевая структура файлов

```
src/
  artifacts.rs          ← НОВЫЙ top-level: верифицированный загрузчик
  anticheat/
    mod.rs              ← фасад: LaunchGuard + реэкспорт публичных типов
    handshake.rs        ← init/confirm/blacklist (нынешнее тело mod.rs)
    hwid.rs             ← без изменений
    scan.rs             ← без изменений
    manifest.rs         ← IntegrityManifest: fetch + выбор SHA по ОС
    agents.rs           ← ensure_agent_jar / ensure_native_agent / native_agent_target
    inject.rs           ← декларативная сборка JVM-аргументов + контракт флаг-файлов
    kick.rs             ← KickReason: чтение kick-файла + сообщение игроку + KICK_PREFIX
  main.rs               ← −≈250 строк античита; launch_profile зовёт LaunchGuard
```

### Границы доменов

- **`artifacts.rs`** — общий низкоуровневый загрузчик (verify + download + retry + cache).
  **Не зависит** от `anticheat`. Используется и `anticheat::agents`, и `ensure_authlib_injector`
  (который остаётся в main/yggdrasil-слое). Так authlib не тянет зависимость от античита.
  HTTP-клиент передаётся параметром (DI): handshake/blacklist используют клиент с auth,
  manifest/агенты — без auth. Фабрики `http_client()` / `download_client()` остаются в `main.rs`.
- **`anticheat/`** — античит-домен: handshake, hwid, scan, manifest целостности, управление агентами,
  инжект, kick. Внешний интерфейс — узкий фасад `LaunchGuard`.
- **Остаётся в `main.rs` без изменений** (yggdrasil-домен, связан лишь через nonce):
  `fetch_yggdrasil_session`, `session_keepalive_loop`, `invalidate_yggdrasil_session`, UI-alert.
  `ensure_authlib_injector` остаётся в main, но переписывается поверх `artifacts::ensure`.

### Ключевые интерфейсы

**`artifacts.rs`**

```rust
pub enum IntegrityError { Tampered(String) }   // подмена → блок запуска
pub fn verify_sha(path: &Path, expected: Option<&str>) -> Result<(), IntegrityError>;
pub fn ensure(client: &Client, url: &str, path: &Path, dir: &Path,
              expected: Option<&str>) -> Result<Option<PathBuf>, IntegrityError>;
//  Ok(Some) — готов и валиден; Ok(None) — недоступен (fail-open); Err — подмена.
```

**`anticheat/mod.rs` — фасад `LaunchGuard`**

```rust
pub struct LaunchGuard { /* launch_token, nonce, challenge, manifest, kick_file */ }

impl LaunchGuard {
    pub fn begin(config: &AppConfig, token: &str) -> Result<Self, String>;
    //  handshake/init + scan процессов + hwid + ОДИН fetch манифеста целостности
    pub fn nonce(&self) -> &str;                 // для fetch_yggdrasil_session
    pub fn authlib_sha(&self) -> Option<&str>;   // для authlib-инжекта в main
    pub fn inject_into(&mut self, jvm_args: &mut Vec<String>,
                       config: &AppConfig) -> Result<(), String>;  // native + agent.jar
    pub fn finish(&self) -> Option<kick::KickReason>;  // разбор kick-файла после игры
}
```

Манифест целостности тянется **один раз** в `begin()` и хранится в guard. Поэтому `authlib_sha()`
доступен main'у, а `inject_into` берёт SHA агентов из того же манифеста — никакого двойного fetch.

**`anticheat/handshake.rs` — явная fail-open модель**

```rust
enum InitOutcome {
    Allowed { token: String, nonce: String, challenge: String },
    Blocked(String),         // бан HWID/аккаунта (403) → блок запуска
    UpdateRequired(String),  // 426 → форс-апдейт, блок запуска
    Unavailable,             // сеть/сбой → FAIL-OPEN: запуск без enforcement
}
```

`begin()` мапит `Unavailable` в guard с пустым `launch_token` (эквивалент нынешнего `Network → empty GuardOk`).
Каждая fail-open точка помечена комментарием.

**`anticheat/inject.rs` — декларативная сборка**

```rust
pub struct InjectionPlan { pub args: Vec<String>, pub kick_file: Option<PathBuf> }

pub fn build(token: &str, challenge: &str, manifest: Option<&IntegrityManifest>,
             config: &AppConfig, data_dir: &Path) -> Result<InjectionPlan, String>;
//  ensure native + agent.jar (через artifacts), чистка флаг/events/kick файлов,
//  сборка Vec<String> аргументов агентов; затем splice в начало jvm_args.
```

`build` принимает конкретные входы, а не весь `LaunchGuard` — это убирает обратную зависимость
`inject` → `mod` и делает функцию юнит-тестируемой без конструирования guard. Метод-обёртка
`LaunchGuard::inject_into` вызывает `inject::build(&self.launch_token, &self.challenge,
self.manifest.as_ref(), …)`, забирает `plan.kick_file` в своё поле и splice-ает `plan.args` в начало.

Контракт флаг-файлов — константы в одном месте:

```rust
const NATIVE_FLAG: &str = "ac_native.flag";
const NATIVE_EVENTS: &str = "ac_native.flag.events";
const KICK_FLAG: &str = "ac_kick.flag";
```

**`anticheat/kick.rs`**

```rust
pub const KICK_PREFIX: &str = "\u{1}ANTICHEAT_KICK\u{1}";
pub struct KickReason(String);
impl KickReason {
    pub fn read_from(path: &Path) -> Option<Self>;  // парсит `reason=` из kick-файла
    pub fn into_alert(self) -> String;              // KICK_PREFIX + текст игроку
}
```

`KICK_PREFIX` остаётся `pub` — `main.rs` (play-handler) детектит его через `strip_prefix`,
чтобы показать полноэкранное уведомление вместо обычной ошибки.

## Что НЕ меняется (поведение)

- **fail-open сохраняется** во всех точках (manifest=None, `ensure`=Ok(None), handshake Unavailable).
- **Последовательность enforcement** та же: pre-launch guard → yggdrasil-сессия по nonce → инжект →
  запуск → keepalive → kick-разбор.
- **Набор JVM-аргументов тот же.** Единственное отклонение от «1-в-1»: внутренний порядок античит-флагов
  становится читаемым (native-флаги, затем agent-флаги) вместо нынешнего порядка от двойного `insert`.
  Для JVM порядок этих `-D` / `-javaagent` / `-agentpath` до главного класса не важен.
  Проверяется реальным запуском (см. ниже).

## Тестирование и верификация

**Юнит-тесты** (естественный бонус от выделения чистой логики):
- `manifest` — выбор SHA по текущей ОС;
- `inject::build` — состав аргументов на фейковых путях (наличие `-Dac.token`, `-agentpath`, и т.д.);
- `kick::KickReason` — парсинг `reason=` и формирование текста алерта;
- `artifacts::verify_sha` — совпадение/несовпадение/отсутствие ожидаемого хэша.
- HWID-тесты уже есть, сохраняются.

**Верификация эквивалентности:**
- `cargo build`, `cargo test`, `cargo clippy` — зелёные;
- **обязательно один реальный запуск игры** на модовом профиле (NeoForge): инжект агентов нельзя
  проверить юнит-тестом — только живой JVM. Проверить, что игра стартует и kick-механика работает.

## Риски и последующие шаги

- **Регрессия инжекта** — главный риск. Митигируется сохранением функционально эквивалентного набора
  аргументов и реальным запуском перед мержем.
- **Выкатка.** Изменение затрагивает поведение запуска → при релизе нужен новый билд лаунчера и его раздача
  (сначала как необязательный, затем mandatory — по обычному workflow релизов). Это вне scope рефакторинга,
  но учитывается при деплое. Версия в `launcher-slint/Cargo.toml` (сейчас 0.3.4) бампится на этапе релиза.

## Вне scope

- Изменение политики fail-open → fail-closed.
- Перенос yggdrasil-сессии/keepalive в отдельный модуль.
- P5 (NeoForge-мод), включение attestation — отдельные задачи.
