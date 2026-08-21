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
		// AutoMigrate may change a result column type while pgx still holds an
		// implicit prepared plan for the same table. PostgreSQL then rejects the
		// first post-migration query with "cached plan must not change result
		// type". Simple protocol keeps schema migration + immediate startup safe
		// and also works through transaction-pooling proxies.
		dialect := postgres.New(postgres.Config{DSN: cfg.DatabaseURL, PreferSimpleProtocol: true})
		db, err := gorm.Open(dialect, &gorm.Config{})
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
		&models.PolicyConsent{},
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
		&models.BotMenuMessage{},
		&models.BotPasswordReset{},
		&models.BotSupportTicket{},
		&models.Session{},
		// Yggdrasil: игровые сессии переживают рестарт backend.
		&models.YggdrasilSession{},
		&models.YggdrasilJoin{},
		// Релизы лаунчера (автообновление).
		&models.LauncherRelease{},
		&models.LauncherReleaseFile{},
		// Delivery v2: immutable releases, chunked CAS and durable jobs.
		&models.DeliveryBlob{},
		&models.ProfileRelease{},
		&models.ProfileReleaseFile{},
		&models.ProfileReleaseFileChunk{},
		&models.LauncherDeliveryArtifact{},
		&models.LauncherDeliveryArtifactChunk{},
		&models.DeliveryJob{},
		// Скриншоты экранов игроков (античит-запросы от админа).
		&models.Screenshot{},
		// Игровой прогресс PJM BaseMod (пишет мод, читают дашборд и бот).
		&models.PlayerProfile{},
		&models.PlayerXpAdjustment{},
		&models.GameKitCatalog{},
		// Оплаченные заказы сайта (YooKassa → ручная/автоматическая выдача).
		&models.Order{},
		&models.ShopItem{},
		&models.Delivery{},
	)
}
