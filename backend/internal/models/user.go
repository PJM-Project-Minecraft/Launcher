package models

import "time"

// User — объединённый аккаунт лаунчера и Telegram-бота.
// ID (uuid) — единственный первичный ключ; для локально зарегистрированных игроков
// он равен offline-UUID Minecraft (OfflinePlayer:<login>), который совпадает с ProviderUUID.
// Поля Email/PasswordHash/Telegram*/TOTP*/IsBanned пришли из Telegram-бота: теперь
// логин лаунчера может валидироваться локально (LocalProvider, bcrypt+TOTP).
type User struct {
	ID           string `gorm:"type:uuid;primaryKey" json:"id"`
	Login        string `gorm:"size:64;uniqueIndex;not null" json:"login"`
	ProviderUUID string `gorm:"size:64;uniqueIndex;not null" json:"providerUuid"`
	Email        string `gorm:"size:255;index" json:"email"`
	// PasswordHash — bcrypt-хеш; пуст для пользователей, приходящих через внешний HTTP-провайдер.
	PasswordHash string `gorm:"size:255" json:"-"`
	IsSlim       bool   `gorm:"not null;default:false" json:"isSlim"`
	// Role — единый набор в нижнем регистре: user | moderator | admin.
	Role string `gorm:"size:16;not null;default:user" json:"role"`

	// Привязка к Telegram (бот).
	TelegramID       *int64     `gorm:"uniqueIndex" json:"telegramId,omitempty"`
	TelegramUsername string     `gorm:"size:64" json:"telegramUsername,omitempty"`
	TelegramLinkedAt *time.Time `json:"telegramLinkedAt,omitempty"`

	// Двухфакторная аутентификация (TOTP).
	TOTPSecret  string `gorm:"size:64" json:"-"`
	TOTPEnabled bool   `gorm:"not null;default:false" json:"totpEnabled"`

	// Блокировки и последний вход.
	IsBanned     bool   `gorm:"not null;default:false" json:"isBanned"`
	IsHwidBanned bool   `gorm:"not null;default:false" json:"isHwidBanned"`
	HardwareID   string `gorm:"size:512" json:"-"`
	IPAddress    string `gorm:"size:64" json:"-"`

	// Согласие с Политикой конфиденциальности: версия принятого документа
	// (0 — не принимал) и момент принятия. История — в PolicyConsent.
	PolicyAcceptedVersion int        `gorm:"not null;default:0" json:"policyAcceptedVersion"`
	PolicyAcceptedAt      *time.Time `json:"policyAcceptedAt,omitempty"`

	LastLoginAt *time.Time `json:"lastLoginAt"`
	CreatedAt   time.Time  `json:"createdAt"`
	UpdatedAt   time.Time  `json:"updatedAt"`
}

// Роли в единой схеме (нижний регистр).
const (
	RoleUser      = "user"
	RoleModerator = "moderator"
	RoleAdmin     = "admin"
)

// IsPrivileged — есть ли у пользователя доступ к админ-функциям (admin или moderator).
func (u User) IsPrivileged() bool {
	return u.Role == RoleAdmin || u.Role == RoleModerator
}
