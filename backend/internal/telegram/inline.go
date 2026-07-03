package telegram

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"time"
)

// InlineBtn — кнопка inline-клавиатуры: либо callback (Data), либо ссылка (URL).
type InlineBtn struct {
	Text string
	Data string // callback_data (взаимоисключимо с URL; URL приоритетнее)
	URL  string
}

// InlineMarkup собирает reply_markup c inline_keyboard из рядов кнопок.
func InlineMarkup(rows ...[]InlineBtn) map[string]any {
	keyboard := make([]any, 0, len(rows))
	for _, row := range rows {
		r := make([]any, 0, len(row))
		for _, b := range row {
			o := map[string]any{"text": b.Text}
			if b.URL != "" {
				o["url"] = b.URL
			} else {
				o["callback_data"] = b.Data
			}
			r = append(r, o)
		}
		keyboard = append(keyboard, r)
	}
	return map[string]any{"inline_keyboard": keyboard}
}

// LinkPreviewBanner — link_preview_options для баннера над текстом меню.
// Пустой url отключает превью совсем (чтобы случайные ссылки в тексте не разворачивались).
func LinkPreviewBanner(url string) map[string]any {
	if url == "" {
		return map[string]any{"is_disabled": true}
	}
	return map[string]any{
		"url":                url,
		"prefer_large_media": true,
		"show_above_text":    true,
	}
}

// postBotAPIResult — как postBotAPI, но возвращает result для разбора (message_id и т.п.).
func postBotAPIResult(client *http.Client, token, method string, payload map[string]any) (json.RawMessage, error) {
	if client == nil {
		client = http.DefaultClient
	}
	raw, err := json.Marshal(payload)
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		fmt.Sprintf("https://api.telegram.org/bot%s/%s", token, method), bytes.NewReader(raw))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	var buf bytes.Buffer
	_, _ = buf.ReadFrom(resp.Body)
	var env struct {
		OK          bool            `json:"ok"`
		Description string          `json:"description"`
		Result      json.RawMessage `json:"result"`
	}
	if err := json.Unmarshal(buf.Bytes(), &env); err != nil {
		if resp.StatusCode < 200 || resp.StatusCode >= 300 {
			return nil, fmt.Errorf("telegram %s HTTP %d: %s", method, resp.StatusCode, buf.String())
		}
		return nil, fmt.Errorf("telegram %s: не удалось разобрать ответ: %v", method, err)
	}
	if !env.OK {
		return nil, fmt.Errorf("telegram %s: %s", method, env.Description)
	}
	return env.Result, nil
}

// SendMessageHTMLWithID — sendMessage (HTML), возвращает message_id отправленного
// сообщения (нужен, чтобы потом редактировать меню на месте).
func SendMessageHTMLWithID(client *http.Client, token string, chatID int64, html string, replyMarkup, linkPreview map[string]any) (int, error) {
	body := map[string]any{
		"chat_id":    chatID,
		"text":       html,
		"parse_mode": "HTML",
	}
	if replyMarkup != nil {
		body["reply_markup"] = replyMarkup
	}
	if linkPreview != nil {
		body["link_preview_options"] = linkPreview
	}
	res, err := postBotAPIResult(client, token, "sendMessage", body)
	if err != nil {
		return 0, err
	}
	var msg struct {
		MessageID int `json:"message_id"`
	}
	if err := json.Unmarshal(res, &msg); err != nil {
		return 0, fmt.Errorf("telegram sendMessage: message_id: %v", err)
	}
	return msg.MessageID, nil
}

// EditMessageTextHTML — editMessageText (HTML) с inline-клавиатурой и превью.
// Ошибку «message is not modified» отдаёт как есть — вызывающий решает, игнорировать ли.
func EditMessageTextHTML(client *http.Client, token string, chatID int64, messageID int, html string, replyMarkup, linkPreview map[string]any) error {
	body := map[string]any{
		"chat_id":    chatID,
		"message_id": messageID,
		"text":       html,
		"parse_mode": "HTML",
	}
	if replyMarkup != nil {
		body["reply_markup"] = replyMarkup
	}
	if linkPreview != nil {
		body["link_preview_options"] = linkPreview
	}
	_, err := postBotAPIResult(client, token, "editMessageText", body)
	return err
}

// AnswerCallbackQuery снимает «часики» с нажатой inline-кнопки;
// text != "" показывает тост (showAlert=true — модальное окно).
func AnswerCallbackQuery(client *http.Client, token string, callbackID string, text string, showAlert bool) error {
	body := map[string]any{"callback_query_id": callbackID}
	if text != "" {
		body["text"] = text
		body["show_alert"] = showAlert
	}
	return postBotAPI(client, token, "answerCallbackQuery", body)
}
