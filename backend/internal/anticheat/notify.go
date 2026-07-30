package anticheat

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"sync"
	"time"

	"launcher-backend/internal/models"
)

// Notifier отправляет алерт о детекте во внешний канал (Telegram и т.п.).
type Notifier interface {
	NotifyDetection(d models.Detection, autoBanned bool)
	// NotifyAgentSilent — мягкий детект: агент перестал слать heartbeat во время игры.
	// Сессию это НЕ гасит (её держит keepalive лаунчера) — лишь сигнал для оператора.
	NotifyAgentSilent(nonce string)
}

// TelegramNotifier шлёт алерты в Telegram напрямую через Bot API.
// Токен может принадлежать любому боту (на проде — vps-ops-bot).
type TelegramNotifier struct {
	token  string
	chatID string
	client *http.Client

	// Тип/сигнатуру детекта задаёт клиент, а лимитер детектов — per-UUID: с нескольких
	// аккаунтов канал алертов забивался мусором, и настоящий детект терялся в шуме.
	// Глобальный потолок — единственное место, через которое проходят ВСЕ алерты.
	mu         sync.Mutex
	sentAt     []time.Time
	suppressed int
}

// alertsPerMinute — сколько алертов в минуту пропускаем в чат. Живой поток детектов
// на проде — единицы в минуту, так что потолок бьёт только по флуду.
const alertsPerMinute = 15

// allow сообщает, можно ли отправить алерт, и сколько было подавлено с прошлой отправки.
func (n *TelegramNotifier) allow() (bool, int) {
	now := time.Now()
	n.mu.Lock()
	defer n.mu.Unlock()
	kept := n.sentAt[:0]
	for _, t := range n.sentAt {
		if now.Sub(t) < time.Minute {
			kept = append(kept, t)
		}
	}
	n.sentAt = kept
	if len(n.sentAt) >= alertsPerMinute {
		n.suppressed++
		return false, 0
	}
	n.sentAt = append(n.sentAt, now)
	suppressed := n.suppressed
	n.suppressed = 0
	return true, suppressed
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

	n.send(text)
}

// NotifyAgentSilent шлёт мягкий алерт: heartbeat агента пропал во время игры. Это не
// kill (вход держит keepalive лаунчера) — повод присмотреться к игроку, а не бан.
func (n *TelegramNotifier) NotifyAgentSilent(nonce string) {
	short := nonce
	if len(short) > 12 {
		short = short[:12] + "…"
	}
	n.send("⚠️ Античит: агент перестал слать heartbeat во время игры (nonce " + short +
		"). Сессия не погашена (её держит лаунчер) — присмотритесь к игроку.")
}

// send отправляет произвольный текст в Telegram-чат алертов (сверх потолка — дропает,
// не блокирует: иначе на флуде копились бы goroutine).
func (n *TelegramNotifier) send(text string) {
	ok, suppressed := n.allow()
	if !ok {
		return
	}
	if suppressed > 0 {
		text += fmt.Sprintf("\n\n(подавлено алертов из-за флуда: %d)", suppressed)
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
		// В URL лежит токен бота, а *url.Error печатает URL целиком — логируем причину.
		var ue *url.Error
		if errors.As(err, &ue) {
			err = ue.Err
		}
		slog.Warn("anticheat: telegram alert failed", "error", err)
		return
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		slog.Warn("anticheat: telegram alert rejected", "status", resp.StatusCode)
	}
}
