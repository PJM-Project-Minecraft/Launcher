// Package policy — Политика конфиденциальности: текст документа (go:embed),
// текущая версия и учёт согласий пользователей. Enforcement живёт в местах
// использования: anticheat/handshake/init (451) и гейты Telegram-бота.
package policy

import (
	"context"
	_ "embed"
	"time"

	"launcher-backend/internal/models"

	"gorm.io/gorm"
)

// Version — текущая версия политики. Содержательная правка privacy.md
// обязана бампать версию: все пользователи пройдут согласие заново.
const Version = 1

// Updated — дата последней редакции (показывается клиентам).
const Updated = "2026-07-04"

// Источники согласия для журнала PolicyConsent.
const (
	SourceLauncher = "launcher"
	SourceBot      = "bot"
)

//go:embed privacy.md
var text string

// Text возвращает канонический markdown-текст политики.
func Text() string { return text }

// Status — блок о политике в ответах API (логин).
type Status struct {
	Required bool `json:"required"`
	Version  int  `json:"version"`
}

// NeedsConsent — принял ли пользователь текущую версию политики.
func NeedsConsent(u *models.User) bool {
	return u.PolicyAcceptedVersion < Version
}

// StatusFor — статус согласия для ответа логина.
func StatusFor(u *models.User) Status {
	return Status{Required: NeedsConsent(u), Version: Version}
}

// RecordConsent фиксирует согласие: обновляет поля пользователя и добавляет
// запись в append-only журнал. Обе записи — в одной транзакции.
func RecordConsent(ctx context.Context, db *gorm.DB, userID, source, ip string) error {
	now := time.Now().UTC()
	return db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		if err := tx.Model(&models.User{}).Where("id = ?", userID).Updates(map[string]any{
			"policy_accepted_version": Version,
			"policy_accepted_at":      now,
		}).Error; err != nil {
			return err
		}
		return tx.Create(&models.PolicyConsent{
			UserID:     userID,
			Version:    Version,
			AcceptedAt: now,
			Source:     source,
			IP:         ip,
		}).Error
	})
}
