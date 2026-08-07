package models

import "time"

// PlayerProfile — снимок игрового прогресса игрока PJM BaseMod. Пишется ТОЛЬКО модом
// (upsert по UUID из /api/game/players/sync); дашборд и бот читают. Авторитетный
// источник — SavedData игрового сервера, здесь зеркало для отображения вне игры.
type PlayerProfile struct {
	UUID      string    `gorm:"type:uuid;primaryKey" json:"uuid"`
	Name      string    `gorm:"size:64;index" json:"name"`
	XP        int64     `gorm:"not null;default:0" json:"xp"`
	RankID    string    `gorm:"size:64" json:"rankId"`
	UpdatedAt time.Time `json:"updatedAt"`
}

// PlayerXpAdjustment — очередь правок XP извне игры. Дашборд/бот НЕ трогают
// PlayerProfile.XP: они кладут сюда запись, мод её забирает поллингом, применяет
// и ACK-ает (applied_at). Это снимает необходимость разрешать конфликты записи.
//
// SetValue != nil — «выставить ровно столько», иначе применяется Delta.
type PlayerXpAdjustment struct {
	ID        uint       `gorm:"primaryKey" json:"id"`
	UUID      string     `gorm:"type:uuid;index" json:"uuid"`
	Delta     int64      `gorm:"not null;default:0" json:"delta"`
	SetValue  *int64     `json:"setValue,omitempty"`
	Reason    string     `gorm:"size:255" json:"reason"`
	CreatedBy string     `gorm:"size:64" json:"createdBy"`
	CreatedAt time.Time  `json:"createdAt"`
	AppliedAt *time.Time `gorm:"index" json:"appliedAt,omitempty"`
}
