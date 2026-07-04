package models

import "time"

// PolicyConsent — журнал согласий с Политикой конфиденциальности (append-only):
// юридический след — кто, когда, какую версию и откуда принял.
type PolicyConsent struct {
	ID         uint      `gorm:"primaryKey" json:"id"`
	UserID     string    `gorm:"type:uuid;index;not null" json:"userId"`
	Version    int       `gorm:"not null" json:"version"`
	AcceptedAt time.Time `gorm:"not null" json:"acceptedAt"`
	Source     string    `gorm:"size:16;not null" json:"source"` // launcher | bot
	IP         string    `gorm:"size:64" json:"-"`
}
