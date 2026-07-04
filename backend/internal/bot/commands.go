package bot

import "launcher-backend/internal/telegram"

// BotMenuCommands — язык по умолчанию. В подсказках намеренно только /menu:
// вся навигация живёт в inline-меню, остальные команды работают, но не рекламируются.
func BotMenuCommands() []telegram.BotCommand {
	return []telegram.BotCommand{
		{Command: "menu", Description: "🏠 Главное меню"},
	}
}

// BotMenuCommandsEN — для клиентов Telegram с языком интерфейса English.
func BotMenuCommandsEN() []telegram.BotCommand {
	return []telegram.BotCommand{
		{Command: "menu", Description: "🏠 Main menu"},
	}
}
