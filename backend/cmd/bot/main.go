// Команда bot — Telegram-бот учётных записей. Делит общую БД с backend (cmd/server),
// поэтому подключение берётся из того же конфигурационного слоя (PostgreSQL/SQLite).
package main

import (
	"log/slog"
	"net/http"
	"os"
	"time"

	"launcher-backend/internal/bot"
	"launcher-backend/internal/botconfig"
	"launcher-backend/internal/config"
	"launcher-backend/internal/database"
	"launcher-backend/internal/launcherrelease"
	"launcher-backend/internal/telegram"

	tele "gopkg.in/telebot.v3"
)

func main() {
	_ = os.Setenv("GOTRACEBACK", "all")
	log := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelInfo}))
	if err := run(log); err != nil {
		log.Error("fatal", "err", err)
		os.Exit(1)
	}
}

func run(log *slog.Logger) error {
	// Общий конфиг backend даёт строку подключения к БД (DATABASE_URL/SQLITE_PATH).
	cfg := config.Load()
	if err := cfg.Validate(); err != nil {
		return err
	}
	db, err := database.Open(cfg)
	if err != nil {
		return err
	}

	botCfg, err := botconfig.Load()
	if err != nil {
		return err
	}

	httpClient := &http.Client{Timeout: 30 * time.Second}

	if err := telegram.ConfigureBotCommandUI(httpClient, botCfg.TelegramBotToken, bot.BotMenuCommands(), bot.BotMenuCommandsEN()); err != nil {
		log.Warn("меню команд / подсказки Telegram", "err", err)
	} else {
		log.Info("Telegram: подсказки / и меню команд зарегистрированы (scope default+private, ru+en)")
	}

	pref := tele.Settings{
		Token:  botCfg.TelegramBotToken,
		Poller: &tele.LongPoller{Timeout: 30 * time.Second},
	}
	teleBot, err := tele.NewBot(pref)
	if err != nil {
		return err
	}

	svc := &bot.Service{
		DB:       db,
		Cfg:      botCfg,
		HTTP:     httpClient,
		Log:      log,
		Releases: launcherrelease.NewService(db, cfg.LauncherReleaseRoot),
	}
	svc.Attach(teleBot)

	log.Info("Telegram polling…")
	teleBot.Start()
	return nil
}
