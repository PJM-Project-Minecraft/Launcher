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
