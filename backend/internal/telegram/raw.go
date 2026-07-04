package telegram

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"
)

type KeyboardBtn struct {
	Text              string
	Style             string // "primary" | "success" | "danger" или пусто
	IconCustomEmojiID string // optional
}

type ReplyKeyboardStyled struct {
	Rows             [][]KeyboardBtn
	Resize           bool
	InputPlaceholder string
}

func (k *ReplyKeyboardStyled) ToReplyMarkup() map[string]any {
	rowToVals := func(row []KeyboardBtn) []any {
		out := make([]any, 0, len(row))
		for _, b := range row {
			o := map[string]any{"text": b.Text}
			if b.Style != "" {
				o["style"] = b.Style
			}
			if b.IconCustomEmojiID != "" {
				o["icon_custom_emoji_id"] = b.IconCustomEmojiID
			}
			out = append(out, o)
		}
		return out
	}
	keyboard := make([]any, 0, len(k.Rows))
	for _, row := range k.Rows {
		keyboard = append(keyboard, rowToVals(row))
	}
	return map[string]any{
		"keyboard":                keyboard,
		"resize_keyboard":         k.Resize,
		"one_time_keyboard":       false,
		"is_persistent":           true,
		"input_field_placeholder": k.InputPlaceholder,
	}
}

// ReplyKeyboardRemove — reply_markup, снимающий постоянную reply-клавиатуру.
// Нужен, чтобы согнать устаревшую нижнюю клавиатуру (Telegram держит её, пока
// не придёт сообщение с этим markup; inline-меню её не сбрасывает).
func ReplyKeyboardRemove() map[string]any {
	return map[string]any{"remove_keyboard": true}
}

// SendHTTPMessageHTML вызывает Bot API sendMessage с произвольным reply_markup (Bot API 9.4).
func SendHTTPMessageHTML(client *http.Client, token string, chatID int64, text string, replyMarkup map[string]any, parseHTML bool) error {
	if client == nil {
		client = http.DefaultClient
	}
	body := map[string]any{
		"chat_id": chatID,
		"text":    text,
	}
	if parseHTML {
		body["parse_mode"] = "HTML"
	}
	if replyMarkup != nil {
		body["reply_markup"] = replyMarkup
	}
	raw, err := json.Marshal(body)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		fmt.Sprintf("https://api.telegram.org/bot%s/sendMessage", token), bytes.NewReader(raw))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		var buf bytes.Buffer
		_, _ = buf.ReadFrom(resp.Body)
		return fmt.Errorf("telegram HTTP %d: %s", resp.StatusCode, buf.String())
	}
	return nil
}

// SendPhotoPNG вызывает sendPhoto (multipart): PNG и подпись в HTML при непустом caption.
func SendPhotoPNG(client *http.Client, token string, chatID int64, fileName string, png []byte, caption string, replyMarkup map[string]any) error {
	if client == nil {
		client = http.DefaultClient
	}
	var body bytes.Buffer
	mw := multipart.NewWriter(&body)
	if err := mw.WriteField("chat_id", fmt.Sprintf("%d", chatID)); err != nil {
		return err
	}
	part, err := mw.CreateFormFile("photo", fileName)
	if err != nil {
		return err
	}
	if _, err := part.Write(png); err != nil {
		return err
	}
	if caption != "" {
		if err := mw.WriteField("caption", caption); err != nil {
			return err
		}
		if err := mw.WriteField("parse_mode", "HTML"); err != nil {
			return err
		}
	}
	if replyMarkup != nil {
		raw, err := json.Marshal(replyMarkup)
		if err != nil {
			return err
		}
		if err := mw.WriteField("reply_markup", string(raw)); err != nil {
			return err
		}
	}
	ct := mw.FormDataContentType()
	if err := mw.Close(); err != nil {
		return err
	}

	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		fmt.Sprintf("https://api.telegram.org/bot%s/sendPhoto", token), &body)
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", ct)
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	var buf bytes.Buffer
	_, _ = buf.ReadFrom(resp.Body)
	var env apiEnvelope
	if err := json.Unmarshal(buf.Bytes(), &env); err != nil {
		if resp.StatusCode < 200 || resp.StatusCode >= 300 {
			return fmt.Errorf("telegram sendPhoto HTTP %d: %s", resp.StatusCode, buf.String())
		}
		return fmt.Errorf("telegram sendPhoto: разбор ответа: %v", err)
	}
	if !env.OK {
		return fmt.Errorf("telegram sendPhoto: %s", env.Description)
	}
	return nil
}

// SendDocument вызывает sendDocument: файл с диска, подпись caption в HTML при непустой.
// documentFileName — имя файла в Telegram (пусто = по basename filePath).
func SendDocument(client *http.Client, token string, chatID int64, filePath, documentFileName string, caption string, replyMarkup map[string]any) error {
	if client == nil {
		client = http.DefaultClient
	}
	clean := filepath.Clean(filePath)
	st, err := os.Stat(clean)
	if err != nil {
		return fmt.Errorf("launcher file: %w", err)
	}
	if !st.Mode().IsRegular() {
		return fmt.Errorf("launcher file: не обычный файл: %s", clean)
	}
	f, err := os.Open(clean)
	if err != nil {
		return fmt.Errorf("launcher file: %w", err)
	}
	defer f.Close()

	var body bytes.Buffer
	mw := multipart.NewWriter(&body)
	if err := mw.WriteField("chat_id", fmt.Sprintf("%d", chatID)); err != nil {
		return err
	}
	baseName := strings.TrimSpace(documentFileName)
	if baseName == "" {
		baseName = filepath.Base(clean)
	}
	part, err := mw.CreateFormFile("document", baseName)
	if err != nil {
		return err
	}
	if _, err := io.Copy(part, f); err != nil {
		return err
	}
	if caption != "" {
		if err := mw.WriteField("caption", caption); err != nil {
			return err
		}
		if err := mw.WriteField("parse_mode", "HTML"); err != nil {
			return err
		}
	}
	if replyMarkup != nil {
		raw, err := json.Marshal(replyMarkup)
		if err != nil {
			return err
		}
		if err := mw.WriteField("reply_markup", string(raw)); err != nil {
			return err
		}
	}
	ct := mw.FormDataContentType()
	if err := mw.Close(); err != nil {
		return err
	}

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Minute)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		fmt.Sprintf("https://api.telegram.org/bot%s/sendDocument", token), &body)
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", ct)
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	var buf bytes.Buffer
	_, _ = buf.ReadFrom(resp.Body)
	var env apiEnvelope
	if err := json.Unmarshal(buf.Bytes(), &env); err != nil {
		if resp.StatusCode < 200 || resp.StatusCode >= 300 {
			return fmt.Errorf("telegram sendDocument HTTP %d: %s", resp.StatusCode, buf.String())
		}
		return fmt.Errorf("telegram sendDocument: разбор ответа: %v", err)
	}
	if !env.OK {
		return fmt.Errorf("telegram sendDocument: %s", env.Description)
	}
	return nil
}

// BotCommand элемент меню команд бота (setMyCommands).
type BotCommand struct {
	Command     string `json:"command"`
	Description string `json:"description"`
}

type apiEnvelope struct {
	OK          bool   `json:"ok"`
	Description string `json:"description"`
}

func postBotAPI(client *http.Client, token, method string, payload map[string]any) error {
	if client == nil {
		client = http.DefaultClient
	}
	raw, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		fmt.Sprintf("https://api.telegram.org/bot%s/%s", token, method), bytes.NewReader(raw))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	var buf bytes.Buffer
	_, _ = buf.ReadFrom(resp.Body)
	var env apiEnvelope
	if err := json.Unmarshal(buf.Bytes(), &env); err != nil {
		if resp.StatusCode < 200 || resp.StatusCode >= 300 {
			return fmt.Errorf("telegram %s HTTP %d: %s", method, resp.StatusCode, buf.String())
		}
		return fmt.Errorf("telegram %s: не удалось разобрать ответ: %v", method, err)
	}
	if !env.OK {
		return fmt.Errorf("telegram %s: %s", method, env.Description)
	}
	return nil
}

// SetMyCommands регистрирует команды для области по умолчанию (все контексты, где не задан узкий scope).
func SetMyCommands(client *http.Client, token string, commands []BotCommand) error {
	return postBotAPI(client, token, "setMyCommands", map[string]any{"commands": commands})
}

// SetMyCommandsScoped — то же с явным Bot API scope (например all_private_chats).
func SetMyCommandsScoped(client *http.Client, token string, commands []BotCommand, scope map[string]any) error {
	body := map[string]any{"commands": commands}
	if scope != nil {
		body["scope"] = scope
	}
	return postBotAPI(client, token, "setMyCommands", body)
}

// SetMyCommandsForLanguage — локализованный список (например languageCode "en"). См. Bot API setMyCommands.
func SetMyCommandsForLanguage(client *http.Client, token string, languageCode string, commands []BotCommand) error {
	return postBotAPI(client, token, "setMyCommands", map[string]any{
		"commands":      commands,
		"language_code": languageCode,
	})
}

// SetMyCommandsScopedLanguage — список команд для языка и scope одновременно.
func SetMyCommandsScopedLanguage(client *http.Client, token string, languageCode string, commands []BotCommand, scope map[string]any) error {
	body := map[string]any{
		"commands":      commands,
		"language_code": languageCode,
	}
	if scope != nil {
		body["scope"] = scope
	}
	return postBotAPI(client, token, "setMyCommands", body)
}

// SetMenuButtonCommands — синяя кнопка «Меню» открывает список команд (как в официальном клиенте).
func SetMenuButtonCommands(client *http.Client, token string) error {
	return postBotAPI(client, token, "setChatMenuButton", map[string]any{
		"menu_button": map[string]any{"type": "commands"},
	})
}

// scopesForHints — подсказки «/» и меню в личке привязаны к scope в клиентах Telegram;
// плюс явная локаль ru, иначе при русском интерфейсе список часто пустой.
func scopesForHints() []map[string]any {
	return []map[string]any{
		{"type": "default"},
		{"type": "all_private_chats"},
	}
}

// ConfigureBotCommandUI: подсказки при вводе «/», кнопка «Меню», локали ru и en.
func ConfigureBotCommandUI(client *http.Client, token string, commandsDefault []BotCommand, commandsEN []BotCommand) error {
	for _, scope := range scopesForHints() {
		if err := SetMyCommandsScoped(client, token, commandsDefault, scope); err != nil {
			return fmt.Errorf("setMyCommands %v: %w", scope, err)
		}
	}
	for _, scope := range scopesForHints() {
		if err := SetMyCommandsScopedLanguage(client, token, "ru", commandsDefault, scope); err != nil {
			return fmt.Errorf("setMyCommands ru %v: %w", scope, err)
		}
	}
	if len(commandsEN) > 0 {
		for _, scope := range scopesForHints() {
			if err := SetMyCommandsScopedLanguage(client, token, "en", commandsEN, scope); err != nil {
				return fmt.Errorf("setMyCommands en %v: %w", scope, err)
			}
		}
	}
	if err := SetMenuButtonCommands(client, token); err != nil {
		return fmt.Errorf("setChatMenuButton: %w", err)
	}
	return nil
}

// DeleteMessage вызывает Bot API deleteMessage (например, чтобы убрать сообщение с паролем из истории).
func DeleteMessage(client *http.Client, token string, chatID int64, messageID int) error {
	if messageID <= 0 {
		return nil
	}
	if client == nil {
		client = http.DefaultClient
	}
	raw, err := json.Marshal(map[string]any{
		"chat_id":    chatID,
		"message_id": messageID,
	})
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		fmt.Sprintf("https://api.telegram.org/bot%s/deleteMessage", token), bytes.NewReader(raw))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	var buf bytes.Buffer
	_, _ = buf.ReadFrom(resp.Body)
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("telegram deleteMessage HTTP %d: %s", resp.StatusCode, buf.String())
	}
	var envelope struct {
		OK          bool `json:"ok"`
		Description string
	}
	if err := json.Unmarshal(buf.Bytes(), &envelope); err == nil && !envelope.OK {
		return fmt.Errorf("telegram deleteMessage: %s", envelope.Description)
	}
	return nil
}
