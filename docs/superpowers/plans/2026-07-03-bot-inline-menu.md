# Гибридное inline-меню Telegram-бота — план реализации

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Заменить reply-клавиатуру на «живое» меню-сообщение с inline-кнопками (редактирование на месте), баннером через link-preview и одной persistent-кнопкой «🏠 Меню».

**Architecture:** Меню — одно сообщение на чат, его `message_id` хранится в новой таблице `bot_menu_messages` (НЕ в `BotDialogue` — `ClearDialogue` делает `DELETE` строки, id не пережил бы завершение диалога; это отклонение от спеки, спека правится в Task 2). Callback-роутинг поверх raw Bot API хелперов (`editMessageText`, `answerCallbackQuery`), рендеры экранов — чистые функции в `menu.go`, диспетчер — `callbacks.go`. Существующие текстовые flow не трогаем — только точки входа (с кнопок) и точки выхода (свежее меню).

**Tech Stack:** Go 1.26 (только через Docker!), telebot.v3, raw Telegram Bot API (внутренний пакет `telegram`), GORM AutoMigrate, Fiber v3 (роут баннера).

## Global Constraints

- Спека: `docs/superpowers/specs/2026-07-03-bot-inline-menu-design.md`.
- **Go на дев-машине нет.** Все `go test`/`go vet` — через Docker:
  ```bash
  docker run --rm -v "$PWD/backend":/src -v launcher_gocache:/root/.cache/go-build \
    -v launcher_gomodcache:/go/pkg/mod -w /src golang:1.26-bookworm go test ./...
  ```
  (далее в шагах — `GO_DOCKER go test …` как сокращение этой обёртки; `$PWD` = корень репо).
- Комментарии в коде — на русском, в стиле существующих файлов.
- Тексты пользователю — русские, HTML parse mode, экранирование через `escHTML`.
- Все текстовые команды (`/start`, `/profile`, `/donate`, `/launcher`, `/2fa`, `/password`, `/email`, `/help`, `/cancel`, `/admin`) и старые лейблы кнопок продолжают работать.
- Callback-data — константы `m:*` (см. Task 3), максимум 64 байта (лимит Telegram).
- Коммиты — по одному на задачу, `feat(bot): …` / `feat(telegram): …`, футер `Co-Authored-By: Claude Fable 5 <noreply@anthropic.com>`.

---

### Task 1: Raw-хелперы Telegram: inline-клавиатуры, link-preview, edit, answerCallback

**Files:**
- Create: `backend/internal/telegram/inline.go`
- Test: `backend/internal/telegram/inline_test.go`
- Modify: `backend/internal/telegram/raw.go` (только добавление функций, существующие не трогать)

**Interfaces:**
- Produces:
  - `type InlineBtn struct { Text, Data, URL string }`
  - `func InlineMarkup(rows ...[]InlineBtn) map[string]any`
  - `func LinkPreviewBanner(url string) map[string]any`
  - `func SendMessageHTMLWithID(client *http.Client, token string, chatID int64, html string, replyMarkup, linkPreview map[string]any) (int, error)`
  - `func EditMessageTextHTML(client *http.Client, token string, chatID int64, messageID int, html string, replyMarkup, linkPreview map[string]any) error`
  - `func AnswerCallbackQuery(client *http.Client, token string, callbackID string, text string, showAlert bool) error`

- [ ] **Step 1: Написать падающий тест на построители разметки**

`backend/internal/telegram/inline_test.go`:

```go
package telegram

import (
	"encoding/json"
	"testing"
)

// TestInlineMarkup проверяет форму inline_keyboard: callback-кнопка несёт
// callback_data, URL-кнопка — url (и не несёт callback_data).
func TestInlineMarkup(t *testing.T) {
	m := InlineMarkup(
		[]InlineBtn{{Text: "Профиль", Data: "m:profile"}, {Text: "Магазин", URL: "https://shop.example"}},
		[]InlineBtn{{Text: "← Назад", Data: "m:home"}},
	)
	raw, err := json.Marshal(m)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var parsed struct {
		Keyboard [][]map[string]string `json:"inline_keyboard"`
	}
	if err := json.Unmarshal(raw, &parsed); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(parsed.Keyboard) != 2 || len(parsed.Keyboard[0]) != 2 || len(parsed.Keyboard[1]) != 1 {
		t.Fatalf("форма клавиатуры: %v", parsed.Keyboard)
	}
	first := parsed.Keyboard[0][0]
	if first["text"] != "Профиль" || first["callback_data"] != "m:profile" {
		t.Errorf("callback-кнопка: %v", first)
	}
	second := parsed.Keyboard[0][1]
	if second["url"] != "https://shop.example" {
		t.Errorf("url-кнопка: %v", second)
	}
	if _, has := second["callback_data"]; has {
		t.Errorf("url-кнопка не должна нести callback_data: %v", second)
	}
}

// TestLinkPreviewBanner: с URL — превью сверху крупно; без URL — превью выключено.
func TestLinkPreviewBanner(t *testing.T) {
	withURL := LinkPreviewBanner("https://x.example/banner.png")
	if withURL["url"] != "https://x.example/banner.png" ||
		withURL["prefer_large_media"] != true || withURL["show_above_text"] != true {
		t.Errorf("banner options: %v", withURL)
	}
	empty := LinkPreviewBanner("")
	if empty["is_disabled"] != true {
		t.Errorf("пустой URL должен отключать превью: %v", empty)
	}
}
```

- [ ] **Step 2: Убедиться, что тест падает**

Run: `GO_DOCKER go test ./internal/telegram/ -run 'TestInlineMarkup|TestLinkPreviewBanner' -v`
Expected: FAIL — `undefined: InlineMarkup` (ошибка компиляции).

- [ ] **Step 3: Реализовать `inline.go`**

`backend/internal/telegram/inline.go`:

```go
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
		return nil, fmt.Errorf("telegram %s: разбор ответа: %v (HTTP %d)", method, err, resp.StatusCode)
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
```

- [ ] **Step 4: Прогнать тесты пакета**

Run: `GO_DOCKER go test ./internal/telegram/ -v`
Expected: PASS (оба новых теста).

- [ ] **Step 5: Commit**

```bash
git add backend/internal/telegram/inline.go backend/internal/telegram/inline_test.go
git commit -m "feat(telegram): inline-клавиатуры, link-preview, editMessageText, answerCallbackQuery"
```

---

### Task 2: Модель BotMenuMessage + repo + правка спеки

**Files:**
- Modify: `backend/internal/models/bot.go` (после `BotDialogue`, ~строка 47)
- Modify: `backend/internal/database/database.go:52` (список AutoMigrate)
- Modify: `backend/internal/repo/repo.go` (после `ClearDialogue`, ~строка 304)
- Modify: `backend/internal/repo/integration_test.go`
- Modify: `docs/superpowers/specs/2026-07-03-bot-inline-menu-design.md`

**Interfaces:**
- Produces:
  - `models.BotMenuMessage{ChatID int64; MessageID int; UpdatedAt time.Time}`
  - `repo.SaveMenuMessage(ctx, db, chatID int64, messageID int) error` (upsert по chat_id)
  - `repo.ReadMenuMessage(ctx, db, chatID int64) (int, error)` — 0, если записи нет

- [ ] **Step 1: Написать падающий интеграционный тест**

В `backend/internal/repo/integration_test.go` добавить (рядом с `TestDialoguePersistence`; хелпер `newTestDB` уже есть в этом файле — SQLite in-memory + `database.AutoMigrate`, поэтому новая таблица подхватится автоматически после Task 2 Step 3):

```go
// TestMenuMessagePersistence: upsert id меню-сообщения по chat_id и чтение;
// отсутствие записи — 0 без ошибки.
func TestMenuMessagePersistence(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()

	got, err := repo.ReadMenuMessage(ctx, db, 77)
	if err != nil || got != 0 {
		t.Fatalf("пустое чтение: got=%d err=%v", got, err)
	}
	if err := repo.SaveMenuMessage(ctx, db, 77, 1001); err != nil {
		t.Fatalf("save: %v", err)
	}
	if err := repo.SaveMenuMessage(ctx, db, 77, 1002); err != nil {
		t.Fatalf("save-2 (upsert): %v", err)
	}
	got, err = repo.ReadMenuMessage(ctx, db, 77)
	if err != nil || got != 1002 {
		t.Fatalf("после upsert: got=%d err=%v", got, err)
	}
}
```

- [ ] **Step 2: Убедиться, что тест падает**

Run: `GO_DOCKER go test ./internal/repo/ -run TestMenuMessagePersistence -v`
Expected: FAIL — `undefined: repo.ReadMenuMessage`.

- [ ] **Step 3: Модель + AutoMigrate + repo-функции**

`backend/internal/models/bot.go`, после `BotDialogue`:

```go
// BotMenuMessage — id последнего «живого» меню-сообщения бота в чате.
// Отдельно от BotDialogue: ClearDialogue удаляет строку диалога целиком,
// а меню должно переживать завершение сценария.
type BotMenuMessage struct {
	ChatID    int64     `gorm:"primaryKey" json:"chatId"`
	MessageID int       `gorm:"not null" json:"messageId"`
	UpdatedAt time.Time `json:"updatedAt"`
}
```

`backend/internal/database/database.go` — в список AutoMigrate после `&models.BotDialogue{},`:

```go
		&models.BotMenuMessage{},
```

`backend/internal/repo/repo.go`, после `ClearDialogue` (import `clause` уже есть):

```go
// --- Меню-сообщение бота ---

func SaveMenuMessage(ctx context.Context, db *gorm.DB, chatID int64, messageID int) error {
	m := models.BotMenuMessage{ChatID: chatID, MessageID: messageID, UpdatedAt: time.Now().UTC()}
	return db.WithContext(ctx).Clauses(clause.OnConflict{
		Columns:   []clause.Column{{Name: "chat_id"}},
		DoUpdates: clause.AssignmentColumns([]string{"message_id", "updated_at"}),
	}).Create(&m).Error
}

func ReadMenuMessage(ctx context.Context, db *gorm.DB, chatID int64) (int, error) {
	var m models.BotMenuMessage
	err := db.WithContext(ctx).Where("chat_id = ?", chatID).First(&m).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return 0, nil
	}
	if err != nil {
		return 0, err
	}
	return m.MessageID, nil
}
```

- [ ] **Step 4: Прогнать тест**

Run: `GO_DOCKER go test ./internal/repo/ -run TestMenuMessagePersistence -v`
Expected: PASS.

- [ ] **Step 5: Поправить спеку (раздел «Отслеживание меню-сообщения»)**

В `docs/superpowers/specs/2026-07-03-bot-inline-menu-design.md` заменить абзац про `MenuMessageID` в модели диалога на:

```markdown
- Запоминаем `message_id` последнего меню на чат в отдельной модели
  `BotMenuMessage` (chat_id PK; GORM AutoMigrate создаст таблицу).
  В `BotDialogue` хранить нельзя: `ClearDialogue` удаляет строку целиком
  (`DELETE`), id меню не пережил бы завершение сценария.
```

- [ ] **Step 6: Commit**

```bash
git add backend/internal/models/bot.go backend/internal/database/database.go \
  backend/internal/repo/repo.go backend/internal/repo/integration_test.go \
  docs/superpowers/specs/2026-07-03-bot-inline-menu-design.md
git commit -m "feat(bot): модель BotMenuMessage — id живого меню-сообщения на чат"
```

---

### Task 3: Рендеры экранов меню (menu.go) — чистые функции

**Files:**
- Create: `backend/internal/bot/menu.go`
- Test: `backend/internal/bot/menu_test.go`

**Interfaces:**
- Consumes: `telegram.InlineBtn`, `telegram.InlineMarkup` (Task 1); `models.User`; `maskEmailUnsafe`, `escHTML` (service.go).
- Produces:
  - Константы callback-data: `cbHome="m:home"`, `cbProfile="m:profile"`, `cbPwd="m:pwd"`, `cbEmail="m:email"`, `cb2FA="m:2fa"`, `cb2FAOn="m:2fa:on"`, `cb2FAOff="m:2fa:off"`, `cbDonate="m:donate"`, `cbLauncher="m:launcher"`, `cbLauncherFile="m:launcher:file"`, `cbLogin="m:login"`, `cbRegister="m:register"`, `cbAdmin="m:admin"`
  - `type menuView struct { User *models.User; Admin bool; Brand, Tagline, DonateURL, LauncherURL string; HasLauncherFile bool }` (User == nil → не привязан)
  - `func buildHomeScreen(v menuView, notice string) (string, map[string]any)`
  - `func buildProfileScreen(v menuView) (string, map[string]any)`
  - `func build2FAScreen(v menuView) (string, map[string]any)`
  - `func buildDonateScreen(v menuView) (string, map[string]any)`
  - `func buildLauncherScreen(v menuView) (string, map[string]any)`
  - `func homeReplyKeyboardMarkup() map[string]any` — reply-клавиатура из одной кнопки `🏠 Меню`
  - Константа `menuButtonLabel = "🏠 Меню"`

- [ ] **Step 1: Написать падающие тесты на сборку экранов**

`backend/internal/bot/menu_test.go`:

```go
package bot

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"launcher-backend/internal/models"
)

// flatButtons разворачивает inline_keyboard в плоский список кнопок для проверок.
func flatButtons(t *testing.T, markup map[string]any) []map[string]string {
	t.Helper()
	raw, err := json.Marshal(markup)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var parsed struct {
		Keyboard [][]map[string]string `json:"inline_keyboard"`
	}
	if err := json.Unmarshal(raw, &parsed); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	var out []map[string]string
	for _, row := range parsed.Keyboard {
		out = append(out, row...)
	}
	return out
}

func hasCallback(btns []map[string]string, data string) bool {
	for _, b := range btns {
		if b["callback_data"] == data {
			return true
		}
	}
	return false
}

func testUser() *models.User {
	return &models.User{
		Login:       "player1",
		Email:       "player1@mail.test",
		Role:        models.RoleUser,
		TOTPEnabled: false,
		CreatedAt:   time.Date(2026, 1, 15, 12, 0, 0, 0, time.UTC),
	}
}

// TestBuildHomeScreenLinked: привязанный не-админ видит 6 кнопок разделов и не видит админку.
func TestBuildHomeScreenLinked(t *testing.T) {
	v := menuView{User: testUser(), Brand: "PJM", Tagline: "t", DonateURL: "https://shop.test"}
	text, markup := buildHomeScreen(v, "")
	btns := flatButtons(t, markup)
	for _, want := range []string{cbProfile, cbPwd, cbEmail, cb2FA, cbDonate, cbLauncher} {
		if !hasCallback(btns, want) {
			t.Errorf("нет кнопки %s", want)
		}
	}
	if hasCallback(btns, cbAdmin) {
		t.Errorf("админка не должна показываться обычному игроку")
	}
	if hasCallback(btns, cbLogin) || hasCallback(btns, cbRegister) {
		t.Errorf("привязанному не показываются Войти/Регистрация")
	}
	if !strings.Contains(text, "player1") {
		t.Errorf("в шапке нет логина: %q", text)
	}
}

// TestBuildHomeScreenAdmin: админу добавляется кнопка админки.
func TestBuildHomeScreenAdmin(t *testing.T) {
	v := menuView{User: testUser(), Admin: true, Brand: "PJM"}
	_, markup := buildHomeScreen(v, "")
	if !hasCallback(flatButtons(t, markup), cbAdmin) {
		t.Errorf("нет кнопки админки")
	}
}

// TestBuildHomeScreenUnlinked: не привязан — Войти/Регистрация + Донат/Лаунчер, без приватных разделов.
func TestBuildHomeScreenUnlinked(t *testing.T) {
	v := menuView{Brand: "PJM"}
	_, markup := buildHomeScreen(v, "")
	btns := flatButtons(t, markup)
	if !hasCallback(btns, cbLogin) || !hasCallback(btns, cbRegister) {
		t.Errorf("нет Войти/Регистрация")
	}
	for _, bad := range []string{cbProfile, cbPwd, cbEmail, cb2FA} {
		if hasCallback(btns, bad) {
			t.Errorf("приватная кнопка %s у не привязанного", bad)
		}
	}
}

// TestBuildHomeScreenNotice: notice попадает в начало текста.
func TestBuildHomeScreenNotice(t *testing.T) {
	v := menuView{User: testUser(), Brand: "PJM"}
	text, _ := buildHomeScreen(v, "✅ Пароль обновлён.")
	if !strings.HasPrefix(text, "✅ Пароль обновлён.") {
		t.Errorf("notice не в начале: %q", text)
	}
}

// TestBuildDonateScreenURLButton: экран доната несёт URL-кнопку магазина и Назад.
func TestBuildDonateScreenURLButton(t *testing.T) {
	v := menuView{User: testUser(), DonateURL: "https://shop.test"}
	_, markup := buildDonateScreen(v)
	btns := flatButtons(t, markup)
	foundURL := false
	for _, b := range btns {
		if b["url"] == "https://shop.test" {
			foundURL = true
		}
	}
	if !foundURL {
		t.Errorf("нет URL-кнопки магазина")
	}
	if !hasCallback(btns, cbHome) {
		t.Errorf("нет кнопки Назад")
	}
}

// TestBuildLauncherScreen: URL-кнопка только при непустом LauncherURL,
// кнопка «файл в чат» — только при HasLauncherFile.
func TestBuildLauncherScreen(t *testing.T) {
	v := menuView{User: testUser(), LauncherURL: "", HasLauncherFile: false}
	_, markup := buildLauncherScreen(v)
	btns := flatButtons(t, markup)
	for _, b := range btns {
		if b["url"] != "" {
			t.Errorf("URL-кнопка при пустом LauncherURL: %v", b)
		}
	}
	if hasCallback(btns, cbLauncherFile) {
		t.Errorf("кнопка файла без файла")
	}

	v2 := menuView{User: testUser(), LauncherURL: "https://dl.test/l.exe", HasLauncherFile: true}
	_, markup2 := buildLauncherScreen(v2)
	btns2 := flatButtons(t, markup2)
	if !hasCallback(btns2, cbLauncherFile) {
		t.Errorf("нет кнопки файла")
	}
}

// TestBuild2FAScreenToggle: выключена — кнопка включения; включена — кнопка выключения.
func TestBuild2FAScreenToggle(t *testing.T) {
	off := menuView{User: testUser()}
	_, markupOff := build2FAScreen(off)
	if !hasCallback(flatButtons(t, markupOff), cb2FAOn) {
		t.Errorf("нет кнопки включения 2FA")
	}
	u := testUser()
	u.TOTPEnabled = true
	on := menuView{User: u}
	_, markupOn := build2FAScreen(on)
	if !hasCallback(flatButtons(t, markupOn), cb2FAOff) {
		t.Errorf("нет кнопки выключения 2FA")
	}
}

// TestHomeReplyKeyboardSingleButton: reply-клавиатура — ровно одна кнопка «🏠 Меню».
func TestHomeReplyKeyboardSingleButton(t *testing.T) {
	raw, _ := json.Marshal(homeReplyKeyboardMarkup())
	var parsed struct {
		Keyboard     [][]map[string]any `json:"keyboard"`
		IsPersistent bool               `json:"is_persistent"`
	}
	if err := json.Unmarshal(raw, &parsed); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(parsed.Keyboard) != 1 || len(parsed.Keyboard[0]) != 1 {
		t.Fatalf("должна быть одна кнопка: %v", parsed.Keyboard)
	}
	if parsed.Keyboard[0][0]["text"] != menuButtonLabel {
		t.Errorf("текст кнопки: %v", parsed.Keyboard[0][0]["text"])
	}
	if !parsed.IsPersistent {
		t.Errorf("клавиатура должна быть persistent")
	}
}
```

- [ ] **Step 2: Убедиться, что тесты падают**

Run: `GO_DOCKER go test ./internal/bot/ -run 'TestBuild|TestHomeReply' -v`
Expected: FAIL — `undefined: menuView` и т.д.

- [ ] **Step 3: Реализовать `menu.go`**

`backend/internal/bot/menu.go`:

```go
package bot

import (
	"fmt"
	"strings"

	"launcher-backend/internal/models"
	"launcher-backend/internal/telegram"
)

// Callback-data экранов inline-меню. Формат "m:<screen>[:<action>]", ≤64 байт.
const (
	cbHome         = "m:home"
	cbProfile      = "m:profile"
	cbPwd          = "m:pwd"
	cbEmail        = "m:email"
	cb2FA          = "m:2fa"
	cb2FAOn        = "m:2fa:on"
	cb2FAOff       = "m:2fa:off"
	cbDonate       = "m:donate"
	cbLauncher     = "m:launcher"
	cbLauncherFile = "m:launcher:file"
	cbLogin        = "m:login"
	cbRegister     = "m:register"
	cbAdmin        = "m:admin"
)

const menuButtonLabel = "🏠 Меню"

// menuView — данные для рендера экранов меню. User == nil — аккаунт не привязан.
type menuView struct {
	User            *models.User
	Admin           bool
	Brand           string
	Tagline         string
	DonateURL       string
	LauncherURL     string
	HasLauncherFile bool
}

func backRow() []telegram.InlineBtn {
	return []telegram.InlineBtn{{Text: "← Назад", Data: cbHome}}
}

// buildHomeScreen — главный экран. notice выводится первой строкой
// (результат завершённого сценария: «Пароль обновлён» и т.п.).
func buildHomeScreen(v menuView, notice string) (string, map[string]any) {
	var b strings.Builder
	if notice != "" {
		b.WriteString(notice + "\n\n")
	}
	b.WriteString("🎮 <b>" + escHTML(v.Brand) + "</b>\n")
	if v.Tagline != "" {
		b.WriteString("<i>" + escHTML(v.Tagline) + "</i>\n")
	}
	b.WriteString("\n")

	if v.User == nil {
		b.WriteString("Привяжите аккаунт, чтобы управлять паролем, почтой и 2FA:\n" +
			"• <b>Войти</b> — учётка уже есть.\n" +
			"• <b>Регистрация</b> — создать новую.\n\n" +
			"<i>Кнопки ниже. Список команд: /help</i>")
		return b.String(), telegram.InlineMarkup(
			[]telegram.InlineBtn{{Text: "🔑 Войти", Data: cbLogin}, {Text: "📋 Регистрация", Data: cbRegister}},
			[]telegram.InlineBtn{{Text: "💎 Донат", Data: cbDonate}, {Text: "⬇ Лаунчер", Data: cbLauncher}},
		)
	}

	totp := "❌"
	if v.User.TOTPEnabled {
		totp = "✅"
	}
	b.WriteString(fmt.Sprintf("👋 Привет, <b>%s</b>!\n", escHTML(v.User.Login)))
	b.WriteString(fmt.Sprintf("👤 %s · 🛡 2FA %s · %s\n\n", escHTML(v.User.Login), totp, escHTML(v.User.Role)))
	b.WriteString("<i>Выберите раздел кнопкой ниже. Список команд: /help</i>")

	rows := [][]telegram.InlineBtn{
		{{Text: "👤 Профиль", Data: cbProfile}, {Text: "🔑 Пароль", Data: cbPwd}},
		{{Text: "📧 Email", Data: cbEmail}, {Text: "🛡 2FA", Data: cb2FA}},
		{{Text: "💎 Донат", Data: cbDonate}, {Text: "⬇ Лаунчер", Data: cbLauncher}},
	}
	if v.Admin {
		rows = append(rows, []telegram.InlineBtn{{Text: "🛠 Админка", Data: cbAdmin}})
	}
	return b.String(), telegram.InlineMarkup(rows...)
}

// buildProfileScreen — карточка профиля (только для привязанных; guard в диспетчере).
func buildProfileScreen(v menuView) (string, map[string]any) {
	u := v.User
	totpLine := "выключена — включите в разделе «2FA»"
	if u.TOTPEnabled {
		totpLine = "включена ✅"
	}
	text := strings.Join([]string{
		"👤 <b>Профиль</b>",
		"",
		fmt.Sprintf("<b>UUID</b>: <code>%s</code>", escHTML(u.ProviderUUID)),
		fmt.Sprintf("<b>Логин</b>: %s", escHTML(u.Login)),
		fmt.Sprintf("<b>Почта</b>: %s", escHTML(maskEmailUnsafe(u.Email))),
		fmt.Sprintf("<b>Роль</b>: %s", escHTML(u.Role)),
		fmt.Sprintf("<b>Регистрация</b>: %s", escHTML(u.CreatedAt.UTC().Format("2006-01-02 15:04:05"))),
		fmt.Sprintf("<b>2FA лаунчера</b>: %s", totpLine),
	}, "\n")
	return text, telegram.InlineMarkup(backRow())
}

// build2FAScreen — статус 2FA и контекстная кнопка включения/выключения.
func build2FAScreen(v menuView) (string, map[string]any) {
	u := v.User
	if u.TOTPEnabled {
		text := "🛡 <b>Двухфакторная защита лаунчера</b>\n\n" +
			"Статус: <b>включена</b> ✅\n" +
			"При входе в лаунчер после пароля запрашивается код из приложения.\n\n" +
			"<i>Отключение попросит пароль и текущий код — защита от случайного сброса.</i>"
		return text, telegram.InlineMarkup(
			[]telegram.InlineBtn{{Text: "Выключить 2FA", Data: cb2FAOff}},
			backRow(),
		)
	}
	text := "🛡 <b>Двухфакторная защита лаунчера</b>\n\n" +
		"Статус: <b>выключена</b> ❌\n" +
		"Включите — и вход в лаунчер потребует код из приложения-аутентификатора.\n\n" +
		"<i>Понадобится Google Authenticator, Authy или аналог.</i>"
	return text, telegram.InlineMarkup(
		[]telegram.InlineBtn{{Text: "Включить 2FA", Data: cb2FAOn}},
		backRow(),
	)
}

// buildDonateScreen — витрина: URL-кнопка прямо в магазин.
func buildDonateScreen(v menuView) (string, map[string]any) {
	text := "💎 <b>Донат и магазин</b>\n\n" +
		"Покупки и оплата проходят на сайте магазина — кнопка ниже.\n\n" +
		"<i>Спасибо за поддержку проекта!</i>"
	rows := [][]telegram.InlineBtn{}
	if v.DonateURL != "" {
		rows = append(rows, []telegram.InlineBtn{{Text: "🛒 Открыть магазин", URL: v.DonateURL}})
	}
	rows = append(rows, backRow())
	return text, telegram.InlineMarkup(rows...)
}

// buildLauncherScreen — скачивание лаунчера: прямая ссылка и/или файл в чат.
func buildLauncherScreen(v menuView) (string, map[string]any) {
	text := "⬇ <b>Лаунчер</b>\n\n" +
		"Скачайте по прямой ссылке или получите файл прямо в чат.\n\n" +
		"<i>Вход в лаунчер — учётка сайта (привязка в этом боте).</i>"
	rows := [][]telegram.InlineBtn{}
	if v.LauncherURL != "" {
		rows = append(rows, []telegram.InlineBtn{{Text: "🌐 Скачать с сайта", URL: v.LauncherURL}})
	}
	if v.HasLauncherFile {
		rows = append(rows, []telegram.InlineBtn{{Text: "📩 Прислать файл в чат", Data: cbLauncherFile}})
	}
	if v.LauncherURL == "" && !v.HasLauncherFile {
		text = "⬇ <b>Лаунчер</b>\n\nРаздача лаунчера сейчас не настроена. Обратитесь к администратору проекта."
	}
	rows = append(rows, backRow())
	return text, telegram.InlineMarkup(rows...)
}

// homeReplyKeyboardMarkup — постоянная reply-клавиатура из одной кнопки «🏠 Меню».
func homeReplyKeyboardMarkup() map[string]any {
	k := &telegram.ReplyKeyboardStyled{
		Rows:             [][]telegram.KeyboardBtn{{{Text: menuButtonLabel, Style: "primary"}}},
		Resize:           true,
		InputPlaceholder: "«🏠 Меню» — главный экран, /help — команды",
	}
	return k.ToReplyMarkup()
}
```

- [ ] **Step 4: Прогнать тесты**

Run: `GO_DOCKER go test ./internal/bot/ -run 'TestBuild|TestHomeReply' -v`
Expected: PASS (все 8).

- [ ] **Step 5: Commit**

```bash
git add backend/internal/bot/menu.go backend/internal/bot/menu_test.go
git commit -m "feat(bot): рендеры экранов inline-меню и reply-клавиатура из одной кнопки"
```

---

### Task 4: Отправка/редактирование меню + конфиг баннера

**Files:**
- Modify: `backend/internal/botconfig/config.go`
- Modify: `backend/internal/bot/menu.go` (добавить методы Service)

**Interfaces:**
- Consumes: Task 1 (`SendMessageHTMLWithID`, `EditMessageTextHTML`, `LinkPreviewBanner`, `DeleteMessage`), Task 2 (`repo.SaveMenuMessage`, `repo.ReadMenuMessage`), Task 3 (рендеры).
- Produces:
  - `botconfig.Config.BotBannerURL string` (env `BOT_BANNER_URL`)
  - `(s *Service) menuViewFor(telegramUID int64) (menuView, error)`
  - `(s *Service) sendHomeMenu(chatID, telegramUID int64, notice string) error` — шлёт свежее меню, удаляет старое, сохраняет id
  - `(s *Service) editMenuScreen(chatID int64, messageID int, text string, markup map[string]any) error` — edit с fallback на новое сообщение

- [ ] **Step 1: Конфиг**

В `backend/internal/botconfig/config.go`: в struct `Config` после `PublicOrigin string` добавить `BotBannerURL string`; в `Load()` после `PublicOrigin: …` добавить:

```go
		BotBannerURL:              envTrim("BOT_BANNER_URL"),
```

- [ ] **Step 2: Методы Service в `menu.go`**

Добавить в конец `backend/internal/bot/menu.go` (импорт `"launcher-backend/internal/repo"` добавить):

```go
// menuViewFor собирает данные экрана для пользователя Telegram.
func (s *Service) menuViewFor(telegramUID int64) (menuView, error) {
	u, err := repo.FindUserByTelegram(s.ctx(), s.DB, telegramUID)
	if err != nil {
		return menuView{}, err
	}
	adm, err := s.resolveAdmin(telegramUID)
	if err != nil {
		return menuView{}, err
	}
	return menuView{
		User:            u,
		Admin:           adm != nil,
		Brand:           s.Cfg.BrandPublicName,
		Tagline:         s.Cfg.BrandTagline,
		DonateURL:       s.Cfg.DonateShopURL,
		LauncherURL:     s.Cfg.LauncherDirectDownloadURL(),
		HasLauncherFile: s.launcherExePath() != "",
	}, nil
}

// bannerPreview — link_preview_options меню (nil нельзя: пустой URL отключает превью).
func (s *Service) bannerPreview() map[string]any {
	return telegram.LinkPreviewBanner(strings.TrimSpace(s.Cfg.BotBannerURL))
}

// sendHomeMenu шлёт свежее меню-сообщение (главный экран) внизу чата,
// удаляет предыдущее меню и запоминает новый message_id.
func (s *Service) sendHomeMenu(chatID, telegramUID int64, notice string) error {
	v, err := s.menuViewFor(telegramUID)
	if err != nil {
		return err
	}
	text, markup := buildHomeScreen(v, notice)
	if old, err := repo.ReadMenuMessage(s.ctx(), s.DB, chatID); err == nil && old > 0 {
		// Старое меню убираем, чтобы в истории не жило два «живых» меню.
		_ = telegram.DeleteMessage(s.HTTP, s.Cfg.TelegramBotToken, chatID, old)
	}
	id, err := telegram.SendMessageHTMLWithID(s.HTTP, s.Cfg.TelegramBotToken, chatID, text, markup, s.bannerPreview())
	if err != nil {
		return err
	}
	return repo.SaveMenuMessage(s.ctx(), s.DB, chatID, id)
}

// editMenuScreen редактирует меню на месте; если сообщение протухло
// (удалено/слишком старое) — шлёт новое и обновляет сохранённый id.
func (s *Service) editMenuScreen(chatID int64, messageID int, text string, markup map[string]any) error {
	err := telegram.EditMessageTextHTML(s.HTTP, s.Cfg.TelegramBotToken, chatID, messageID, text, markup, s.bannerPreview())
	if err == nil {
		return repo.SaveMenuMessage(s.ctx(), s.DB, chatID, messageID)
	}
	if strings.Contains(err.Error(), "message is not modified") {
		return nil // повторное нажатие того же экрана — не ошибка
	}
	if s.Log != nil {
		s.Log.Warn("edit меню, пересоздаю", "err", err)
	}
	id, sendErr := telegram.SendMessageHTMLWithID(s.HTTP, s.Cfg.TelegramBotToken, chatID, text, markup, s.bannerPreview())
	if sendErr != nil {
		return sendErr
	}
	return repo.SaveMenuMessage(s.ctx(), s.DB, chatID, id)
}
```

- [ ] **Step 3: Компиляция и весь пакет**

Run: `GO_DOCKER go vet ./... && GO_DOCKER go test ./internal/bot/ ./internal/telegram/ ./internal/repo/`
Expected: PASS, без ошибок vet.

- [ ] **Step 4: Commit**

```bash
git add backend/internal/botconfig/config.go backend/internal/bot/menu.go
git commit -m "feat(bot): отправка и редактирование живого меню, баннер BOT_BANNER_URL"
```

---

### Task 5: Callback-диспетчер + подключение OnCallback

**Files:**
- Create: `backend/internal/bot/callbacks.go`
- Test: `backend/internal/bot/callbacks_test.go`
- Modify: `backend/internal/bot/lifecycle.go` (Attach + извлечение beginPasswordFlow/beginEmailFlow)

**Interfaces:**
- Consumes: Task 3 константы `cb*` и рендеры; Task 4 `menuViewFor`/`editMenuScreen`/`sendHomeMenu`; существующие `beginLoginFlow(chatID, *tele.User)`, `beginRegisterFlow(chatID, *tele.User)`, `beginTotpFlow(chatID, telegramUID)`, `replyLauncherDownload`, `resolveAdmin`, `adminOpsKeyboardMarkup`.
- Produces:
  - `func normalizeCallbackData(raw string) string` — trim `\f`-префикса telebot и пробелов
  - `func callbackNeedsLink(data string) bool` — приватные экраны (`m:profile`, `m:pwd`, `m:email`, `m:2fa*`, `m:admin`)
  - `(s *Service) HandleCallback(c tele.Context) error`
  - `(s *Service) beginPasswordFlow(chatID, telegramUID int64) error`, `(s *Service) beginEmailFlow(chatID, telegramUID int64) error` — извлечены из `idleActions` (lifecycle.go:187–214), используются и текстовым, и callback-путём

- [ ] **Step 1: Написать падающий тест на чистую логику диспетчера**

`backend/internal/bot/callbacks_test.go`:

```go
package bot

import "testing"

// TestNormalizeCallbackData: telebot добавляет "\f" к data — срезаем.
func TestNormalizeCallbackData(t *testing.T) {
	cases := map[string]string{
		"m:home":       "m:home",
		"\fm:profile":  "m:profile",
		" m:donate ":   "m:donate",
		"\f m:2fa:on ": "m:2fa:on",
	}
	for in, want := range cases {
		if got := normalizeCallbackData(in); got != want {
			t.Errorf("normalizeCallbackData(%q) = %q, want %q", in, got, want)
		}
	}
}

// TestCallbackNeedsLink: приватные экраны требуют привязки, публичные — нет.
func TestCallbackNeedsLink(t *testing.T) {
	private := []string{cbProfile, cbPwd, cbEmail, cb2FA, cb2FAOn, cb2FAOff, cbAdmin}
	public := []string{cbHome, cbDonate, cbLauncher, cbLauncherFile, cbLogin, cbRegister}
	for _, d := range private {
		if !callbackNeedsLink(d) {
			t.Errorf("%s должен требовать привязку", d)
		}
	}
	for _, d := range public {
		if callbackNeedsLink(d) {
			t.Errorf("%s не должен требовать привязку", d)
		}
	}
}
```

- [ ] **Step 2: Убедиться, что тест падает**

Run: `GO_DOCKER go test ./internal/bot/ -run 'TestNormalizeCallbackData|TestCallbackNeedsLink' -v`
Expected: FAIL — `undefined: normalizeCallbackData`.

- [ ] **Step 3: Извлечь beginPasswordFlow/beginEmailFlow из idleActions**

В `backend/internal/bot/lifecycle.go` тела case-веток `"Сменить пароль", "/password"` (строки 187–201) и `"Email", "/email"` (строки 203–214) вынести в методы; в `idleActions` от веток остаются вызовы `return s.beginPasswordFlow(chatID, telegramUID)` / `return s.beginEmailFlow(chatID, telegramUID)`:

```go
// beginPasswordFlow запускает сценарий смены пароля (текстовые шаги как раньше).
func (s *Service) beginPasswordFlow(chatID, telegramUID int64) error {
	if uidPtr, err := s.linkedUID(telegramUID); err != nil {
		return err
	} else if uidPtr == nil {
		return s.forbidNotLinked(chatID, telegramUID)
	}
	ep := repo.EmptyPayload()
	_ = repo.SaveDialogue(s.ctx(), s.DB, chatID, repo.FlowChangePwdOld, &ep)
	return s.notifyHTML(chatID, s.msgWithCancelHint(
		"Смена пароля в два шага:\n"+
			"1) Сейчас пришлите <b>текущий пароль</b> (его сообщение будет удалено из чата и заменено заглушкой).\n"+
			"2) Затем бот пришлёт <b>шестизначный код</b> в этот чат — введите его.\n"+
			"3) После этого пришлите <b>новый пароль</b> (8–128 символов).\n\n"+
			"<i>Если забыли пароль — обратитесь к администратору проекта.</i>"),
		homeReplyKeyboardMarkup())
}

// beginEmailFlow запускает сценарий смены почты.
func (s *Service) beginEmailFlow(chatID, telegramUID int64) error {
	if uidPtr, err := s.linkedUID(telegramUID); err != nil {
		return err
	} else if uidPtr == nil {
		return s.forbidNotLinked(chatID, telegramUID)
	}
	ep := repo.EmptyPayload()
	_ = repo.SaveDialogue(s.ctx(), s.DB, chatID, repo.FlowChangeEmailAsk, &ep)
	return s.notifyHTML(chatID, s.msgWithCancelHint(
		"Укажите новый <b>адрес e-mail</b> одним сообщением (тот, который будет храниться в аккаунте).\n\n"+
			"После этого бот пришлёт код в чат — его нужно будет ввести, чтобы подтвердить смену."),
		homeReplyKeyboardMarkup())
}
```

(Обратить внимание: `keyboardRemove()` здесь уже заменён на `homeReplyKeyboardMarkup()` — кнопка «🏠 Меню» остаётся доступной во время сценария. Остальные замены — Task 6.)

- [ ] **Step 4: Реализовать `callbacks.go`**

`backend/internal/bot/callbacks.go`:

```go
package bot

import (
	"strings"

	"launcher-backend/internal/repo"
	"launcher-backend/internal/telegram"

	tele "gopkg.in/telebot.v3"
)

// normalizeCallbackData срезает служебный префикс "\f" telebot и пробелы.
func normalizeCallbackData(raw string) string {
	return strings.TrimSpace(strings.TrimPrefix(raw, "\f"))
}

// callbackNeedsLink — экраны, доступные только привязанному аккаунту.
func callbackNeedsLink(data string) bool {
	switch data {
	case cbProfile, cbPwd, cbEmail, cb2FA, cb2FAOn, cb2FAOff, cbAdmin:
		return true
	}
	return false
}

// answerCb снимает «часики»; ошибки только в лог — это не повод ронять обработку.
func (s *Service) answerCb(id, text string, alert bool) {
	if err := telegram.AnswerCallbackQuery(s.HTTP, s.Cfg.TelegramBotToken, id, text, alert); err != nil && s.Log != nil {
		s.Log.Warn("answerCallbackQuery", "err", err)
	}
}

// HandleCallback — диспетчер нажатий inline-кнопок меню.
func (s *Service) HandleCallback(c tele.Context) error {
	cb := c.Callback()
	if cb == nil || cb.Message == nil || c.Chat() == nil || c.Sender() == nil {
		return nil
	}
	chatID := c.Chat().ID
	telegramUID := telegramUserID(c.Sender())
	msgID := cb.Message.ID
	data := normalizeCallbackData(cb.Data)

	v, err := s.menuViewFor(telegramUID)
	if err != nil {
		s.answerCb(cb.ID, "Ошибка, попробуйте ещё раз", false)
		return err
	}

	if callbackNeedsLink(data) && v.User == nil {
		s.answerCb(cb.ID, "Сначала привяжите аккаунт: «Войти» или «Регистрация».", true)
		text, markup := buildHomeScreen(v, "")
		return s.editMenuScreen(chatID, msgID, text, markup)
	}

	switch data {
	case cbHome:
		s.answerCb(cb.ID, "", false)
		text, markup := buildHomeScreen(v, "")
		return s.editMenuScreen(chatID, msgID, text, markup)

	case cbProfile:
		s.answerCb(cb.ID, "", false)
		text, markup := buildProfileScreen(v)
		return s.editMenuScreen(chatID, msgID, text, markup)

	case cb2FA:
		s.answerCb(cb.ID, "", false)
		text, markup := build2FAScreen(v)
		return s.editMenuScreen(chatID, msgID, text, markup)

	case cbDonate:
		s.answerCb(cb.ID, "", false)
		text, markup := buildDonateScreen(v)
		return s.editMenuScreen(chatID, msgID, text, markup)

	case cbLauncher:
		s.answerCb(cb.ID, "", false)
		text, markup := buildLauncherScreen(v)
		return s.editMenuScreen(chatID, msgID, text, markup)

	case cbLauncherFile:
		s.answerCb(cb.ID, "Отправляю файл…", false)
		return s.replyLauncherDownload(chatID, telegramUID)

	case cbPwd:
		s.answerCb(cb.ID, "", false)
		return s.beginPasswordFlow(chatID, telegramUID)

	case cbEmail:
		s.answerCb(cb.ID, "", false)
		return s.beginEmailFlow(chatID, telegramUID)

	case cb2FAOn, cb2FAOff:
		// beginTotpFlow сам перечитывает состояние — защита от протухшей кнопки.
		s.answerCb(cb.ID, "", false)
		return s.beginTotpFlow(chatID, telegramUID)

	case cbLogin:
		s.answerCb(cb.ID, "", false)
		return s.beginLoginFlow(chatID, c.Sender())

	case cbRegister:
		s.answerCb(cb.ID, "", false)
		return s.beginRegisterFlow(chatID, c.Sender())

	case cbAdmin:
		adm, err := s.resolveAdmin(telegramUID)
		if err != nil {
			return err
		}
		if adm == nil {
			s.answerCb(cb.ID, "Панель только для модераторов.", true)
			return nil
		}
		s.answerCb(cb.ID, "", false)
		ep := repo.EmptyPayload()
		_ = repo.SaveDialogue(s.ctx(), s.DB, chatID, repo.FlowAdminMenu, &ep)
		return s.notifyHTML(chatID,
			"<b>Панель администратора</b>\n"+
				"• «🔍 Поиск» — найти игрока по нику, логину, почте или id.\n"+
				"• «📡 OPS» — краткий дайджест сервисов (если настроено в .env).\n"+
				"• «⬅ Выйти» — вернуться к обычному меню.",
			s.adminOpsKeyboardMarkup())

	default:
		// Кнопка из старой версии меню — просто пересоздаём главный экран.
		s.answerCb(cb.ID, "Меню обновилось", false)
		return s.sendHomeMenu(chatID, telegramUID, "")
	}
}
```

В `Attach` (`lifecycle.go:12-19`) добавить после обработчика OnText:

```go
	bot.Handle(tele.OnCallback, func(c tele.Context) error {
		if err := s.HandleCallback(c); err != nil && s.Log != nil {
			s.Log.Error("callback", "err", err)
		}
		return nil
	})
```

- [ ] **Step 5: Прогнать тесты и vet**

Run: `GO_DOCKER go vet ./... && GO_DOCKER go test ./internal/bot/ -v`
Expected: PASS (включая новые TestNormalizeCallbackData, TestCallbackNeedsLink).

- [ ] **Step 6: Commit**

```bash
git add backend/internal/bot/callbacks.go backend/internal/bot/callbacks_test.go backend/internal/bot/lifecycle.go
git commit -m "feat(bot): callback-диспетчер inline-меню и запуск сценариев с кнопок"
```

---

### Task 6: Переключение текстовых путей на новое меню

**Files:**
- Modify: `backend/internal/bot/lifecycle.go`
- Modify: `backend/internal/bot/service.go`
- Modify: `backend/internal/bot/account.go`
- Modify: `backend/internal/bot/totp.go`
- Modify: `backend/internal/bot/link.go`
- Modify: `backend/internal/bot/register.go`
- Modify: `backend/internal/bot/admin.go`

**Interfaces:**
- Consumes: `sendHomeMenu`, `homeReplyKeyboardMarkup`, `menuButtonLabel` (Tasks 3–4).
- Produces: удаляются `mainKeyboardMarkup` и `keyboardRemove` (все вызовы замещены).

Правила замены (механические, по всем файлам пакета bot):

1. **`kb, err := s.mainKeyboardMarkup(...)` + проверка err + использование `kb`** → убрать получение kb; финальное сообщение сценария слать через `s.notifyHTML(chatID, …, homeReplyKeyboardMarkup())`, а **после него** вызывать `s.sendHomeMenu(chatID, telegramUID, "")` там, где сценарий завершён. Для коротких результатов вместо пары «сообщение + меню» использовать `s.sendHomeMenu(chatID, telegramUID, "<notice>")` одним сообщением (см. список ниже).
2. **`keyboardRemove()`** → `homeReplyKeyboardMarkup()` (кнопка «🏠 Меню» остаётся во время сценариев). Функцию `keyboardRemove` удалить.
3. **`mainKeyboardMarkup` удалить** вместе с константами лейблов, которые остаются только для обратной совместимости текстовых сообщений (лейблы `labelBtn*`, `donateKeyboardLabel`, `launcherKeyboardLabel` НЕ удалять — их проверяет `idleActions`).

- [ ] **Step 1: lifecycle.go — welcome, onCancel, idleActions, profileCard, cmdHelp**

- `welcome()` (строки 142–160) переписать:

```go
func (s *Service) welcome(chatID int64, telegramUID int64) error {
	_ = repo.ClearDialogue(s.ctx(), s.DB, chatID)
	// Меню несёт inline-клавиатуру; persistent-кнопка «🏠 Меню» выставится
	// первым же обычным сообщением (подсказка сценария, /help и т.п.).
	return s.sendHomeMenu(chatID, telegramUID, "")
}
```

- В `HandleText` в условие `/start` добавить кнопку меню (строка 38):

```go
	if text == "/start" || text == "/menu" || text == menuButtonLabel {
		return s.welcome(chatID, telegramUserID(sender))
	}
```

- `onCancel` (128–140): заменить тело после `ClearDialogue` на

```go
	_ = repo.ClearDialogue(s.ctx(), s.DB, chatID)
	return s.sendHomeMenu(chatID, telegramUserID(sender), "Сценарий сброшен — вы в главном меню.")
```

- `idleActions`: ветки `"Профиль", "/profile"` → `return s.profileCard(chatID, telegramUID)` (остаётся); ветки паролья/email уже делегируют (Task 5); ветку `default` заменить текст: `"Не распознал сообщение.\nНажмите «🏠 Меню» или /start."`.
- `profileCard` (242–272): при `me == nil` — без изменений (notifyWarn); при успехе — вместо сборки текста и `mainKeyboardMarkup` отправлять свежее меню сразу на экране профиля:

```go
	v, err := s.menuViewFor(telegramUID)
	if err != nil {
		return err
	}
	text, markup := buildProfileScreen(v)
	if old, err2 := repo.ReadMenuMessage(s.ctx(), s.DB, chatID); err2 == nil && old > 0 {
		_ = telegram.DeleteMessage(s.HTTP, s.Cfg.TelegramBotToken, chatID, old)
	}
	id, err := telegram.SendMessageHTMLWithID(s.HTTP, s.Cfg.TelegramBotToken, chatID, text, markup, s.bannerPreview())
	if err != nil {
		return err
	}
	return repo.SaveMenuMessage(s.ctx(), s.DB, chatID, id)
```

(импорт `"launcher-backend/internal/telegram"` в lifecycle.go добавить)

- `cmdHelp`: `kb, err := s.mainKeyboardMarkup(…)` → `kb := homeReplyKeyboardMarkup()`; текст `/menu` описать как «показать живое меню»; добавить строку про кнопку «🏠 Меню».

- [ ] **Step 2: service.go — forbidNotLinked, replyLauncherDownload, replyDonateShop, удаление mainKeyboardMarkup/keyboardRemove**

- `forbidNotLinked`: вместо `mainKeyboardMarkup` — свежее меню:

```go
func (s *Service) forbidNotLinked(chatID int64, telegramUID int64) error {
	return s.sendHomeMenu(chatID, telegramUID,
		"⚠ Раздел доступен после привязки аккаунта — «Войти» или «Регистрация».")
}
```

- `replyLauncherDownload` и `replyDonateShop`: `kb, err := s.mainKeyboardMarkup(…)` (+err-check) → `kb := homeReplyKeyboardMarkup()`. (`replyDonateShop` остаётся для команды `/donate`.)
- Удалить `mainKeyboardMarkup`, `keyboardRemove`, `btnWithIconFallback` и `linkedFlag`, если на них не осталось ссылок (проверить `grep -n` перед удалением; `linkedFlag` используется только в `mainKeyboardMarkup`; `btnWithIconFallback` — только там же). Поля `ButtonEmoji*` в botconfig не трогать (конфиг обратносовместим).

- [ ] **Step 3: Завершения сценариев — account.go, totp.go, link.go, register.go**

Каждую точку успешного завершения перевести на `sendHomeMenu(chatID, telegramUID, notice)` (одно сообщение — свежее меню с notice):

- `account.go handlePasswordNew` (101–110): убрать kb-блок и notifyHTML; после `SetPassword` и `ClearDialogue`:
  `return s.sendHomeMenu(chatID, telegramUID, "✅ <b>Пароль обновлён.</b> Используйте его в лаунчере и при входе.")`
  (`ClearDialogue` выполнить ДО sendHomeMenu.)
- `account.go handleChangeEmailOTP` (186–197): аналогично —
  `return s.sendHomeMenu(chatID, telegramUID, fmt.Sprintf("✅ <b>Почта обновлена:</b> <code>%s</code>", escHTML(newMail)))`
- `totp.go handleTotpConfirm` (успех, 125–133): → `return s.sendHomeMenu(chatID, telegramUID, "✅ <b>2FA включена.</b> При входе в лаунчер потребуется код из приложения.")`
- `totp.go handleTotpConfirm` (уже включена, 104–111): kb-блок → `return s.sendHomeMenu(chatID, telegramUID, "2FA уже включена для этого аккаунта.")`
- `totp.go handleTotpDisablePwd` (уже выключена, 181–188): → `return s.sendHomeMenu(chatID, telegramUID, "2FA уже выключена.")`
- `totp.go handleTotpDisableOTP` (успех, 238–246): → `return s.sendHomeMenu(chatID, telegramUID, "✅ <b>2FA отключена.</b> В лаунчере снова достаточно логина и пароля.")`
- `totp.go` `keyboardRemove()` (строки 83, 162, 204) → `homeReplyKeyboardMarkup()`.
- `link.go handleLinkOTP` (успех, 126–134): kb-блок → после `ClearDialogue`:
  `return s.sendHomeMenu(chatID, telegramUserID(sender), "✅ <b>Аккаунт привязан.</b> Теперь доступны профиль, пароль, почта и 2FA.")`
  (сохранить существующий текст успеха, если он информативнее — перенести его в notice).
- `link.go` `keyboardRemove()` (27, 94) → `homeReplyKeyboardMarkup()`.
- `register.go alreadyLinkedChat` (24–29): kb-блок и notifyHTML →
  ```go
  _ = repo.ClearDialogue(s.ctx(), s.DB, chatID)
  _ = s.sendHomeMenu(chatID, tgid, "У вас уже привязан аккаунт к этому Telegram — вход и регистрация не нужны.")
  return true, nil
  ```
- `register.go handleRegOTP` (203–204): kb-блок → `return s.sendHomeMenu(chatID, telegramUserID(sender), "✅ <b>Аккаунт создан и привязан.</b> Добро пожаловать!")`; `keyboardRemove()` (49, 64) → `homeReplyKeyboardMarkup()`.
- `admin.go adminMenuActions`, ветка `"⬅ Выйти из админки"` (101–108): после `ClearDialogue` вместо kb-блока и `notifyHTML("Вы вышли из панели.", kb)` →
  ```go
  return s.sendHomeMenu(chatID, telegramUID, "Вы вышли из панели администратора.")
  ```
- `admin.go`: `keyboardRemove()` (94, 278) → `homeReplyKeyboardMarkup()`. `adminOpsKeyboardMarkup` НЕ трогать.

Точный список всех вызовов перед правкой получить командой:
`grep -n "mainKeyboardMarkup\|keyboardRemove()" backend/internal/bot/*.go`
— после правки эта команда должна вернуть пусто.

- [ ] **Step 4: Компиляция, vet, все тесты бэкенда**

Run: `GO_DOCKER go vet ./... && GO_DOCKER go test ./...`
Expected: PASS. Особо смотреть `internal/bot`, `internal/repo`.

- [ ] **Step 5: Commit**

```bash
git add backend/internal/bot/
git commit -m "feat(bot): все текстовые пути переведены на живое inline-меню"
```

---

### Task 7: Роут баннера на сервере

**Files:**
- Modify: `backend/cmd/server/main.go` (рядом с `/health`, строка ~78)

**Interfaces:**
- Produces: `GET /api/public/bot-banner.png` — отдаёт `data/bot-banner.png`, 404 если файла нет.

- [ ] **Step 1: Добавить роут**

В `backend/cmd/server/main.go` после обработчика `/health` (импорт `"os"` уже есть):

```go
	// Баннер Telegram-бота (link-preview шапка меню). Файл кладётся в data/ руками
	// (scp на прод, как агенты античита); нет файла — 404, бот работает без баннера.
	app.Get("/api/public/bot-banner.png", func(c fiber.Ctx) error {
		const p = "data/bot-banner.png"
		if _, err := os.Stat(p); err != nil {
			return c.SendStatus(fiber.StatusNotFound)
		}
		return c.SendFile(p)
	})
```

- [ ] **Step 2: Проверить компиляцию**

Run: `GO_DOCKER go vet ./cmd/server/`
Expected: без ошибок.

- [ ] **Step 3: Подготовить файл баннера локально (для дев-проверки)**

```bash
cp pjm_discord.png backend/data/bot-banner.png
```

(В git не попадёт — `backend/data/` не отслеживается. Для прода: `scp backend/data/bot-banner.png srv-129:/root/Launcher/backend/data/`. Замечание: картинка 2486×2486 квадратная; для аккуратного «широкого» баннера позже можно подготовить 1200×630 — на работу кода не влияет.)

- [ ] **Step 4: Commit**

```bash
git add backend/cmd/server/main.go
git commit -m "feat(server): публичный роут баннера бота /api/public/bot-banner.png"
```

---

### Task 8: Финальная проверка и ручной прогон

**Files:** нет новых; проверка.

- [ ] **Step 1: Полный CI-прогон как в pipeline**

Run: `GO_DOCKER go vet ./... && GO_DOCKER go test ./...`
Expected: всё PASS.

- [ ] **Step 2: Ручной чек-лист с тестовым ботом**

Запустить бота локально с тестовым токеном (НЕ прод-токен) и SQLite:

```bash
cd backend && TELEGRAM_BOT_TOKEN=<тестовый> BOT_BANNER_URL=<url или пусто> \
  docker run --rm -e TELEGRAM_BOT_TOKEN -e BOT_BANNER_URL \
  -v "$PWD":/src -v launcher_gocache:/root/.cache/go-build \
  -v launcher_gomodcache:/go/pkg/mod -w /src golang:1.26-bookworm go run ./cmd/bot
```

Проверить руками:
1. `/start` — меню-сообщение с inline-кнопками (баннер, если URL задан), внизу одна кнопка «🏠 Меню».
2. Не привязан: «Профиль» из старого меню недоступен; inline-кнопки — Войти/Регистрация; нажатие «Донат» редактирует сообщение на месте, «← Назад» возвращает.
3. Привязка (Войти) с inline-кнопки: текстовый сценарий, по завершении старое меню удалено, внизу свежее меню с notice «Аккаунт привязан».
4. «🔑 Пароль» → сценарий → по завершении меню с notice «Пароль обновлён».
5. «🛡 2FA» → экран статуса → Включить → QR → код → меню с notice.
6. «⬇ Лаунчер» → URL-кнопка (если настроена) и «файл в чат».
7. «🏠 Меню» текстовой кнопкой во время сценария — сброс и главный экран.
8. Нажатие кнопки на старом (удалённом вручную) меню — бот присылает новое меню, не падает.
9. Админский аккаунт — кнопка «🛠 Админка», вход в текстовую админку работает.

- [ ] **Step 3: Commit остатков (если были правки по итогам ручного прогона) и финал**

```bash
git status --short   # убедиться, что не осталось незакоммиченного кода
```

Деплой (отдельно, НЕ в этой задаче): `./deploy.sh`; баннер на прод — scp в `backend/data/`; в прод `.env` добавить `BOT_BANNER_URL=https://launcher.likonchik.xyz/api/public/bot-banner.png` и строку `BOT_BANNER_URL: "${BOT_BANNER_URL:-}"` в `environment:` сервиса `bot` в compose (та же гоча, что с ANTICHEAT_SECRET — без строки в environment переменная до контейнера не дойдёт).
