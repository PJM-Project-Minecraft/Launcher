//! Discord Rich Presence: фоновый актор + чистый маппинг стадий в activity-поля.
//! Полностью опционален: при отсутствии Discord/Client ID лаунчер работает как обычно.

use std::sync::mpsc::{self, Receiver, Sender};
use std::sync::OnceLock;
use std::time::Duration;

use discord_rich_presence::activity::{Activity, Assets, Timestamps};
use discord_rich_presence::{DiscordIpc, DiscordIpcClient};

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
    let mut client = DiscordIpcClient::new(&client_id);
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
