package database

import (
	"log/slog"
	"os"
	"path/filepath"

	"launcher-backend/internal/config"
	"launcher-backend/internal/models"

	"gorm.io/driver/postgres"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
)

func Open(cfg config.Config) (*gorm.DB, error) {
	if cfg.DatabaseURL != "" {
		db, err := gorm.Open(postgres.Open(cfg.DatabaseURL), &gorm.Config{})
		if err != nil {
			return nil, err
		}
		slog.Info("database connected", "driver", "postgres")
		return db, nil
	}

	if err := os.MkdirAll(filepath.Dir(cfg.SQLitePath), 0755); err != nil {
		return nil, err
	}

	db, err := gorm.Open(sqlite.Open(cfg.SQLitePath), &gorm.Config{})
	if err != nil {
		return nil, err
	}
	slog.Info("database connected", "driver", "sqlite", "path", cfg.SQLitePath)
	return db, nil
}

func AutoMigrate(db *gorm.DB) error {
	return db.AutoMigrate(
		&models.User{},
		&models.Profile{},
		&models.GameFile{},
		&models.Detection{},
		&models.Hwid{},
		&models.HwidBan{},
		&models.AccountBan{},
		&models.CheatSignature{},
		// Модели, перенесённые из Telegram-бота:
		&models.AuthLog{},
		&models.TelegramOTP{},
		&models.BotAuditLog{},
		&models.BotDialogue{},
		&models.Session{},
		// Yggdrasil: игровые сессии переживают рестарт backend.
		&models.YggdrasilSession{},
		&models.YggdrasilJoin{},
		// Релизы лаунчера (автообновление).
		&models.LauncherRelease{},
		&models.LauncherReleaseFile{},
		// Скриншоты экранов игроков (античит-запросы от админа).
		&models.Screenshot{},
	)
}
