package anticheat

import (
	"bytes"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"time"

	"launcher-backend/internal/models"
)

// Notifier отправляет алерт о детекте во внешний канал (Telegram и т.п.).
type Notifier interface {
	NotifyDetection(d models.Detection, autoBanned bool)
}

// TelegramNotifier шлёт алерты в Telegram напрямую через Bot API.
// Токен может принадлежать любому боту (на проде — vps-ops-bot).
type TelegramNotifier struct {
	token  string
	chatID string
	client *http.Client
}

func NewTelegramNotifier(token, chatID string) *TelegramNotifier {
	if token == "" || chatID == "" {
		return nil
	}
	return &TelegramNotifier{
		token:  token,
		chatID: chatID,
		client: &http.Client{Timeout: 10 * time.Second},
	}
}

func (n *TelegramNotifier) NotifyDetection(d models.Detection, autoBanned bool) {
	hwid := d.HwidHash
	if len(hwid) > 16 {
		hwid = hwid[:16] + "…"
	}
	text := fmt.Sprintf(
		"🚨 Античит: детект severity %d\n\nИгрок: %s\nТип: %s\nСигнатура: %s\nИсточник: %s\nHWID: %s",
		d.Severity, d.Login, d.Type, d.Signature, d.Source, hwid,
	)
	if autoBanned {
		text += "\n\n⛔️ Выдан автоматический бан (аккаунт + HWID)."
	}

	payload, _ := json.Marshal(map[string]any{
		"chat_id": n.chatID,
		"text":    text,
	})
	resp, err := n.client.Post(
		"https://api.telegram.org/bot"+n.token+"/sendMessage",
		"application/json",
		bytes.NewReader(payload),
	)
	if err != nil {
		slog.Warn("anticheat: telegram alert failed", "error", err)
		return
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		slog.Warn("anticheat: telegram alert rejected", "status", resp.StatusCode)
	}
}
