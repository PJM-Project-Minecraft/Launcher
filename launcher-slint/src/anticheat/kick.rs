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
