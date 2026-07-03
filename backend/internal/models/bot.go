package models

import "time"

// AuthLog — журнал попыток входа (бот, лаунчер, gml-api).
type AuthLog struct {
	ID        uint      `gorm:"primaryKey" json:"id"`
	UserID    *string   `gorm:"type:uuid;index" json:"userId,omitempty"`
	Username  string    `gorm:"size:64;index" json:"username"`
	IP        string    `gorm:"size:64" json:"ip"`
	Source    string    `gorm:"size:64" json:"source"`
	Success   bool      `gorm:"not null" json:"success"`
	Message   string    `gorm:"type:text" json:"message"`
	CreatedAt time.Time `gorm:"index" json:"createdAt"`
}

// TelegramOTP — одноразовые коды для привязки/смены пароля/почты через Telegram.
type TelegramOTP struct {
	ID             string     `gorm:"type:uuid;primaryKey" json:"id"`
	UserID         string     `gorm:"type:uuid;index" json:"userId"`
	CodeHash       string     `gorm:"size:255;not null" json:"-"`
	ExpiresAt      time.Time  `gorm:"index" json:"expiresAt"`
	ConsumedAt     *time.Time `json:"consumedAt,omitempty"`
	TelegramChatID int64      `gorm:"not null" json:"telegramChatId"`
	Purpose        string     `gorm:"size:64;not null" json:"purpose"`

	User User `gorm:"constraint:OnDelete:CASCADE" json:"-"`
}

// BotAuditLog — действия администраторов в боте/панели.
type BotAuditLog struct {
	ID              string    `gorm:"type:uuid;primaryKey" json:"id"`
	AdminTelegramID *int64    `json:"adminTelegramId,omitempty"`
	AdminUserID     *string   `gorm:"type:uuid" json:"adminUserId,omitempty"`
	TargetUserID    *string   `gorm:"type:uuid;index" json:"targetUserId,omitempty"`
	Action          string    `gorm:"size:64;not null" json:"action"`
	Details         string    `gorm:"type:text" json:"details"`
	CreatedAt       time.Time `gorm:"index" json:"createdAt"`
}

// BotDialogue — состояние FSM-диалога Telegram-бота (по chat_id). payload — JSON-текст.
type BotDialogue struct {
	ChatID    int64     `gorm:"primaryKey" json:"chatId"`
	State     string    `gorm:"size:64;not null" json:"state"`
	Payload   string    `gorm:"type:text" json:"payload"`
	UpdatedAt time.Time `json:"updatedAt"`
}

// BotMenuMessage — id последнего «живого» меню-сообщения бота в чате.
// Отдельно от BotDialogue: ClearDialogue удаляет строку диалога целиком,
// а меню должно переживать завершение сценария.
type BotMenuMessage struct {
	ChatID    int64     `gorm:"primaryKey" json:"chatId"`
	MessageID int       `gorm:"not null" json:"messageId"`
	UpdatedAt time.Time `json:"updatedAt"`
}

// Session — сессии веб-аутентификации (зарезервировано, перенос из бота).
type Session struct {
	ID           string    `gorm:"type:uuid;primaryKey" json:"id"`
	SessionToken string    `gorm:"size:255;uniqueIndex;not null" json:"-"`
	UserID       string    `gorm:"type:uuid;index" json:"userId"`
	Expires      time.Time `json:"expires"`

	User User `gorm:"constraint:OnDelete:CASCADE" json:"-"`
}
