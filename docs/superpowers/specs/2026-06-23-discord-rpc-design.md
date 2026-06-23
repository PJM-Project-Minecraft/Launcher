# Discord Rich Presence для launcher-slint

**Дата:** 2026-06-23
**Компонент:** `launcher-slint`

## Цель

Показывать в Discord статус игрока («Rich Presence») с разными стадиями работы
лаунчера: сидит в лаунчере, выбирает сборку, скачивает файлы, играет на сервере
Project: Minecraft. Фича опциональна и не должна влиять на работу лаунчера, если
Discord не запущен.

## Решения (из брейншторма)

- Стадии: **все четыре** — idle, выбор сборки, загрузка, игра.
- Таймер «сколько играет» (elapsed) на стадии игры — **да**, с начала игровой сессии,
  сбрасывается при каждом запуске.
- Ник игрока в presence — **да**, показывается.
- Переключатель в настройках лаунчера — **да**, тогл (default включён).
- Крейт — **`discord-rich-presence`** (чистый Rust, IPC через unix-socket / named pipe,
  без нативных SDK).

## Архитектура

### Модуль `src/discord_rpc.rs`

Фоновый поток-актор владеет соединением. Главный поток (Slint event loop и рабочие
потоки запуска) шлёт ему команды через `mpsc::Sender` — никаких блокировок UI.

```rust
enum Presence {
    Idle,                                   // «В главном меню»
    Browsing { nick: String },              // «Выбирает сборку» / ник
    Downloading { profile: String },        // «Загружает сборку» / имя профиля
    Playing { profile: String, started_at: i64 }, // «Играет на Project: Minecraft»
}

enum RpcCommand {
    Set(Presence),
    SetEnabled(bool),   // тогл из настроек
    Shutdown,
}
```

- Актор держит `Option<DiscordIpcClient>`. При отсутствии Discord — клиент `None`,
  переподключение с экспоненциальным бэкоффом (cap, напр. 30с) при следующей команде
  или по таймеру.
- Последняя запрошенная `Presence` кэшируется: после переподключения актор
  восстанавливает её. При смене стадии, пока Discord закрыт, обновляется только кэш.
- `SetEnabled(false)` → `client.clear_activity()` + перестаёт применять presence,
  пока не придёт `SetEnabled(true)`.
- Все ошибки IPC проглатываются (логируются только под debug). Сбой RPC **никогда**
  не пробрасывается в логику лаунчера.

### Глобальный хэндл

По образцу `UPDATE_SHARED` (`OnceLock<Arc<...>>`): `RPC: OnceLock<Sender<RpcCommand>>`.
Хелперы:

```rust
fn rpc_set(p: Presence);          // отправить Set, no-op если канал не инициализирован
fn rpc_set_enabled(on: bool);
fn rpc_init();                    // создать актор-поток, вызвать один раз в main()
```

`rpc_set` никогда не паникует и не блокирует: `try_send`/`send` в неблокирующий канал,
ошибка отправки игнорируется.

### Client ID

Константа с env-override по образцу `LAUNCHER_DEFAULT_API_URL`:
- В `build.rs` пробрасывается `DISCORD_CLIENT_ID` (env при сборке) в
  `cargo:rustc-env=DISCORD_CLIENT_ID=...`.
- В коде: `option_env!("DISCORD_CLIENT_ID").unwrap_or(DEFAULT_DISCORD_CLIENT_ID)`,
  где `DEFAULT_DISCORD_CLIENT_ID` — реальный Client ID проекта (заполняется значением,
  которое предоставит владелец; до этого — плейсхолдер `"0"`).
- Если Client ID = "0"/пустой → `rpc_init` не запускает актор (фича выключена).

## Контент стадий (presence payload)

Большая иконка везде — `logo` (лого проекта). Маленькая иконка — значок стадии.

| Стадия | details | state | таймер | small image |
|---|---|---|---|---|
| Idle | `В главном меню` | — | — | — |
| Browsing | `Выбирает сборку` | `<ник>` | — | `idle` |
| Downloading | `Загружает сборку` | `<имя профиля>` | — | `download` |
| Playing | `Играет на Project: Minecraft` | `<имя сборки>` | start = `started_at` | `playing` |

**Ключи арт-ассетов в Discord Developer Portal** (загрузить под этими именами):
`logo`, `idle`, `download`, `playing`.

## Точки интеграции в `main.rs`

| Событие | Место в коде | Команда |
|---|---|---|
| Старт приложения | `main()` после `app` создан | `rpc_init()`, затем `rpc_set(Idle)` |
| Вход выполнен | `apply_session` | `rpc_set(Browsing { nick })` |
| Логаут | `register_logout_handler` | `rpc_set(Idle)` |
| Начало подготовки | `sync_and_launch` старт | `rpc_set(Downloading { profile })` |
| Игра запущена | `post_game_started` (после `child.spawn`) | `rpc_set(Playing { profile, started_at = now })` |
| Игра закрыта | после `child.wait()` в `launch_profile` | `rpc_set(Browsing { nick })` |

`started_at` — Unix-время старта игры; берётся в `launch_profile` рядом со `spawn`.
Ник для возврата в `Browsing` после игры — из `RuntimeState.user` (передать в
`launch_profile`/`sync_and_launch` или прочитать из state).

## Настройки

- В `LauncherSettings` добавить `discord_rpc_enabled: bool` с `#[serde(default = "default_discord_rpc_enabled")]`,
  дефолт `true`. Обновить `Default for LauncherSettings`.
- В `ui/app.slint`: булево свойство `discord-rpc-enabled` + чекбокс/тогл в панели
  настроек (по образцу тогла дискретной GPU), колбэк `discord-rpc-requested(bool)`.
- Обработчик `on_discord_rpc_requested` в `register_settings_handler`: сохранить
  настройку, вызвать `rpc_set_enabled(enabled)`, обновить UI-свойство через
  `apply_launcher_settings`.
- В `apply_launcher_settings` — прокинуть значение в `set_discord_rpc_enabled`.
- На старте (`rpc_init`/после загрузки настроек) применить сохранённое значение:
  если выключено — `rpc_set_enabled(false)`.

## Cargo

Добавить в `[dependencies]`: `discord-rich-presence = "0.2"` (зафиксировать
актуальную минорную версию на момент реализации).

## Обработка ошибок

- Discord не установлен/закрыт → актор молча держит `None`, периодически пробует.
- IPC-ошибка во время сессии → сброс клиента в `None`, попытка переподключения.
- Никаких `unwrap`/`expect` на путях RPC. Лаунчер работает идентично и без Discord.

## Тестирование

- `cargo build -p launcher-slint` — компиляция (включает Slint-кодоген).
- Юнит-тест на маппинг `Presence` → payload-поля (details/state/small image) без
  реального IPC: выделить чистую функцию `presence_to_activity_fields(&Presence)`
  и тестировать её.
- Ручная проверка: запустить лаунчер с заданным `DISCORD_CLIENT_ID`, при открытом
  Discord пройти стадии (логин → выбор → запуск → закрытие) и убедиться, что статус
  меняется; выключить тогл — presence исчезает.

## Вне области (YAGNI)

- Кнопки-приглашения / join-в-партию, party size.
- Отдельные иконки на каждую сборку (одна `logo` на всё).
- Презенс с прогресс-процентами загрузки (только текст стадии).
