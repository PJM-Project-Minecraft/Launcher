# Политика конфиденциальности — план реализации

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Обязательное принятие Политики конфиденциальности в лаунчере и Telegram-боте с серверным enforcement (без согласия не выдаётся launch-token) и журналом согласий.

**Architecture:** Новый Go-пакет `internal/policy` (текст через `go:embed`, версия-константа, функции согласия), два поля в `models.User` + append-only журнал `PolicyConsent`. Enforcement — проверка в `anticheat/handshake/init` (451). Лаунчер показывает экран-оверлей после логина, бот — гейт в диспетчере колбэков и шаг согласия в регистрации.

**Tech Stack:** Go + Fiber v3 + GORM, `github.com/yuin/goldmark` (markdown→HTML, новая зависимость), Rust + Slint (лаунчер).

**Spec:** `docs/superpowers/specs/2026-07-04-privacy-policy-design.md`

## Global Constraints

- Go на дев-машине ТОЛЬКО через Docker. Все go-команды гонять так (из корня репо):
  ```bash
  docker run --rm -v "$PWD/backend":/src -v launcher_gocache:/root/.cache/go-build \
    -v launcher_gomodcache:/go/pkg/mod -w /src golang:1.26-bookworm <go-команда>
  ```
  Ниже это сокращено как `$DOCKER_GO <go-команда>`.
- Авторизация в anticheat handler — PER-ROUTE, НЕ `group.Use`. Не менять схему.
- Пакет `policy` НЕ импортирует `auth` (иначе цикл: auth импортирует policy для блока в LoginResult). Доступ к текущему юзеру в handler'е policy — через инжектируемую функцию.
- Slint: layout как один из нескольких детей не фиксирует ширину — вёрстка нового экрана только абсолютным позиционированием (`x`/`width` явно).
- Flow-константы бота (`repo.FlowState`) — iota, значения персистятся в БД. Новые состояния НЕ добавляем (план обходится без них).
- Тексты пользователю — на русском, стиль существующих сообщений бота (HTML-разметка, эмодзи-префиксы).
- Коммиты после каждой задачи; сообщения в стиле репо: `feat(policy): ...`, `feat(bot): ...`, `feat(launcher): ...`.

---

### Task 1: Модель данных — поля согласия и журнал

**Files:**
- Modify: `backend/internal/models/user.go`
- Create: `backend/internal/models/policy_consent.go`
- Modify: `backend/internal/database/database.go` (функция `AutoMigrate`, строка ~39)

**Interfaces:**
- Produces: `models.User.PolicyAcceptedVersion int`, `models.User.PolicyAcceptedAt *time.Time`, тип `models.PolicyConsent`. На них опираются задачи 2–7.

- [ ] **Step 1: Добавить поля в User**

В `backend/internal/models/user.go` после блока «Блокировки и последний вход» (после поля `IPAddress`, перед `LastLoginAt`) добавить:

```go
	// Согласие с Политикой конфиденциальности: версия принятого документа
	// (0 — не принимал) и момент принятия. История — в PolicyConsent.
	PolicyAcceptedVersion int        `gorm:"not null;default:0" json:"policyAcceptedVersion"`
	PolicyAcceptedAt      *time.Time `json:"policyAcceptedAt,omitempty"`
```

- [ ] **Step 2: Создать модель журнала**

Создать `backend/internal/models/policy_consent.go`:

```go
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
```

- [ ] **Step 3: Добавить в AutoMigrate**

В `backend/internal/database/database.go` в список `db.AutoMigrate(...)` после `&models.User{},` добавить строку:

```go
		&models.PolicyConsent{},
```

- [ ] **Step 4: Проверить сборку и тесты**

```bash
$DOCKER_GO go vet ./...
$DOCKER_GO go test ./internal/database/ ./internal/repo/
```
Expected: PASS (существующие интеграционные тесты прогоняют AutoMigrate с новой моделью).

- [ ] **Step 5: Commit**

```bash
git add backend/internal/models/ backend/internal/database/
git commit -m "feat(policy): поля согласия у User + журнал PolicyConsent"
```

---

### Task 2: Пакет policy — текст, версия, логика согласия

**Files:**
- Create: `backend/internal/policy/privacy.md`
- Create: `backend/internal/policy/service.go`
- Test: `backend/internal/policy/service_test.go`

**Interfaces:**
- Consumes: `models.User`, `models.PolicyConsent` (Task 1).
- Produces (используют задачи 3–7):
  - `policy.Version int = 1`, `policy.Updated string = "2026-07-04"`
  - `policy.Text() string` — сырой markdown
  - `policy.NeedsConsent(u *models.User) bool`
  - `policy.RecordConsent(ctx context.Context, db *gorm.DB, userID, source, ip string) error`
  - `policy.StatusFor(u *models.User) policy.Status` где `Status{Required bool; Version int}`
  - константы `policy.SourceLauncher = "launcher"`, `policy.SourceBot = "bot"`

- [ ] **Step 1: Написать черновик privacy.md**

Создать `backend/internal/policy/privacy.md` (полный черновик; владелец вычитает перед релизом):

```markdown
# Политика конфиденциальности

**Версия 1 · действует с 4 июля 2026 г.**

Настоящая Политика описывает, какие данные собирает и обрабатывает проект
Project Minecraft (далее — «Проект»): игровой лаунчер, игровой сервер,
сайт и Telegram-бот Проекта. Используя лаунчер, играя на сервере Проекта или
пользуясь Telegram-ботом, вы принимаете условия этой Политики.

## 1. Какие данные мы собираем

**Данные аккаунта:** игровой логин (никнейм), адрес электронной почты,
пароль (хранится только в виде необратимого хеша), роль на проекте,
дата регистрации, история входов.

**Данные Telegram:** идентификатор (Telegram ID) и имя пользователя
(username) привязанного Telegram-аккаунта.

**Технические данные:** идентификатор оборудования (HWID — хеши компонентов
компьютера), IP-адрес, версия лаунчера и операционной системы.

**Игровые данные:** игровые сессии (время входа и выхода, сервер),
данные, необходимые для работы автоматической защиты (античита).

**Данные античита, включая скриншоты экрана:** во время игровой сессии
система защиты может собирать сведения о запущенных модулях и модификациях
игры, а также **делать снимки (скриншоты) экрана** по запросу администрации
Проекта. Скриншоты снимаются только во время активной игровой сессии и
используются исключительно для проверки соблюдения правил честной игры.

## 2. Зачем мы обрабатываем данные

- работа аккаунта: вход в лаунчер и на сервер, восстановление доступа;
- защита от читов, мультиаккаунтов и обхода блокировок (HWID, IP, античит,
  скриншоты экрана);
- уведомления и управление аккаунтом через Telegram-бота;
- безопасность и стабильность сервиса.

## 3. Скриншоты экрана

Запрос скриншота инициирует администрация Проекта при подозрении на
нарушение правил. Снимок делается процессом лаунчера во время игры,
передаётся на сервер Проекта по защищённому соединению и доступен только
администрации. Скриншоты не публикуются и удаляются после завершения
проверки, за исключением случаев, когда они служат доказательством
нарушения правил Проекта.

## 4. Где и сколько хранятся данные

Данные хранятся на сервере Проекта. Данные аккаунта и журнал согласий
хранятся весь срок существования аккаунта. Скриншоты и телеметрия античита
хранятся ограниченное время, необходимое для проверки, после чего удаляются.

## 5. Кому мы передаём данные

Никому. Данные не продаются и не передаются третьим лицам, за исключением
случаев, прямо предусмотренных законом.

## 6. Ваши права

Вы можете запросить копию своих данных, исправление неточностей или
удаление аккаунта вместе с данными — напишите администрации через
Telegram-бота Проекта. Удаление аккаунта делает игру на сервере Проекта
невозможной.

## 7. Изменения Политики

При существенном изменении Политики её версия повышается, и лаунчер и
Telegram-бот попросят принять новую редакцию. Продолжение использования
Проекта без принятия новой редакции невозможно.

## 8. Контакты

По вопросам обработки данных обращайтесь к администрации через
Telegram-бота Проекта (раздел «Меню»).
```

- [ ] **Step 2: Написать падающий тест**

Создать `backend/internal/policy/service_test.go`:

```go
package policy

import (
	"context"
	"testing"

	"launcher-backend/internal/database"
	"launcher-backend/internal/models"

	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
)

func openTestDB(t *testing.T) *gorm.DB {
	t.Helper()
	db, err := gorm.Open(sqlite.Open("file::memory:"), &gorm.Config{})
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	if err := database.AutoMigrate(db); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	return db
}

func TestNeedsConsent(t *testing.T) {
	if !NeedsConsent(&models.User{PolicyAcceptedVersion: 0}) {
		t.Error("нулевая версия должна требовать согласие")
	}
	if NeedsConsent(&models.User{PolicyAcceptedVersion: Version}) {
		t.Error("актуальная версия не должна требовать согласие")
	}
}

func TestStatusFor(t *testing.T) {
	st := StatusFor(&models.User{PolicyAcceptedVersion: 0})
	if !st.Required || st.Version != Version {
		t.Errorf("StatusFor = %+v, want Required=true Version=%d", st, Version)
	}
}

func TestRecordConsent(t *testing.T) {
	db := openTestDB(t)
	u := models.User{ID: "11111111-1111-1111-1111-111111111111", Login: "player", ProviderUUID: "11111111-1111-1111-1111-111111111111"}
	if err := db.Create(&u).Error; err != nil {
		t.Fatalf("create user: %v", err)
	}

	if err := RecordConsent(context.Background(), db, u.ID, SourceLauncher, "1.2.3.4"); err != nil {
		t.Fatalf("RecordConsent: %v", err)
	}

	var saved models.User
	if err := db.First(&saved, "id = ?", u.ID).Error; err != nil {
		t.Fatalf("reload user: %v", err)
	}
	if saved.PolicyAcceptedVersion != Version || saved.PolicyAcceptedAt == nil {
		t.Errorf("user = ver %d at %v, want ver %d и непустое время", saved.PolicyAcceptedVersion, saved.PolicyAcceptedAt, Version)
	}

	var consents []models.PolicyConsent
	if err := db.Find(&consents).Error; err != nil {
		t.Fatalf("read consents: %v", err)
	}
	if len(consents) != 1 || consents[0].Source != SourceLauncher || consents[0].Version != Version || consents[0].IP != "1.2.3.4" {
		t.Errorf("журнал = %+v, want одна запись launcher/v%d/1.2.3.4", consents, Version)
	}
}

func TestTextNotEmpty(t *testing.T) {
	if len(Text()) < 500 {
		t.Errorf("текст политики подозрительно короткий: %d байт", len(Text()))
	}
}
```

- [ ] **Step 3: Убедиться, что тест падает**

```bash
$DOCKER_GO go test ./internal/policy/
```
Expected: FAIL — пакет не компилируется (нет service.go).

- [ ] **Step 4: Реализовать service.go**

Создать `backend/internal/policy/service.go`:

```go
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
```

- [ ] **Step 5: Тесты зелёные**

```bash
$DOCKER_GO go test ./internal/policy/
```
Expected: PASS.

- [ ] **Step 6: Commit**

```bash
git add backend/internal/policy/
git commit -m "feat(policy): пакет policy — текст v1, версия, учёт согласий"
```

---

### Task 3: HTTP-роуты policy + страница /privacy

**Files:**
- Create: `backend/internal/policy/handler.go`
- Test: `backend/internal/policy/handler_test.go`
- Modify: `backend/cmd/server/main.go` (после строки 107 `auth.NewHandler(...)`)
- Modify: `backend/go.mod` (новая зависимость goldmark)

**Interfaces:**
- Consumes: `policy.Version/Updated/Text/RecordConsent` (Task 2); `auth.CurrentUser` передаётся снаружи (НЕ импортировать auth в policy).
- Produces: `GET /api/policy` → `{"version":1,"updatedAt":"2026-07-04","text":"..."}`; `POST /api/policy/accept` (JWT, тело `{"version":1}`) → 204, при несовпадении версии → 409 `{"message":..., "version":N}`; `GET /privacy` → HTML. Их используют задачи 8–9 (лаунчер) и кнопки бота.

- [ ] **Step 1: Добавить зависимость goldmark**

```bash
docker run --rm -v "$PWD/backend":/src -v launcher_gocache:/root/.cache/go-build \
  -v launcher_gomodcache:/go/pkg/mod -w /src golang:1.26-bookworm \
  sh -c "go get github.com/yuin/goldmark@v1.7.8 && go mod tidy"
```
Expected: `go.mod`/`go.sum` обновлены.

- [ ] **Step 2: Написать падающий тест**

Создать `backend/internal/policy/handler_test.go`:

```go
package policy

import (
	"encoding/json"
	"io"
	"net/http/httptest"
	"strings"
	"testing"

	"launcher-backend/internal/models"

	"github.com/gofiber/fiber/v3"
	"gorm.io/gorm"
)

// newTestApp поднимает fiber с policy-роутами; тестовый middleware подкладывает
// юзера напрямую (боевой RequireAuth живёт в auth и здесь не нужен).
func newTestApp(t *testing.T, db *gorm.DB, user *models.User) *fiber.App {
	t.Helper()
	app := fiber.New()
	requireAuth := func(c fiber.Ctx) error {
		if user == nil {
			return c.SendStatus(401)
		}
		return c.Next()
	}
	currentUser := func(c fiber.Ctx) (models.User, bool) {
		if user == nil {
			return models.User{}, false
		}
		return *user, true
	}
	NewHandler(db).RegisterRoutes(app, requireAuth, currentUser)
	return app
}

func TestGetPolicy(t *testing.T) {
	app := newTestApp(t, openTestDB(t), nil)
	res, err := app.Test(httptest.NewRequest("GET", "/api/policy", nil))
	if err != nil {
		t.Fatalf("app.Test: %v", err)
	}
	if res.StatusCode != 200 {
		t.Fatalf("status = %d, want 200", res.StatusCode)
	}
	var body struct {
		Version   int    `json:"version"`
		UpdatedAt string `json:"updatedAt"`
		Text      string `json:"text"`
	}
	if err := json.NewDecoder(res.Body).Decode(&body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if body.Version != Version || body.UpdatedAt != Updated || len(body.Text) < 500 {
		t.Errorf("body = v%d %q len(text)=%d", body.Version, body.UpdatedAt, len(body.Text))
	}
}

func TestAcceptPolicy(t *testing.T) {
	db := openTestDB(t)
	u := models.User{ID: "22222222-2222-2222-2222-222222222222", Login: "p2", ProviderUUID: "22222222-2222-2222-2222-222222222222"}
	if err := db.Create(&u).Error; err != nil {
		t.Fatalf("create user: %v", err)
	}
	app := newTestApp(t, db, &u)

	req := httptest.NewRequest("POST", "/api/policy/accept", strings.NewReader(`{"version":1}`))
	req.Header.Set("Content-Type", "application/json")
	res, err := app.Test(req)
	if err != nil {
		t.Fatalf("app.Test: %v", err)
	}
	if res.StatusCode != 204 {
		t.Fatalf("status = %d, want 204", res.StatusCode)
	}
	var saved models.User
	if err := db.First(&saved, "id = ?", u.ID).Error; err != nil {
		t.Fatalf("reload: %v", err)
	}
	if saved.PolicyAcceptedVersion != Version {
		t.Errorf("PolicyAcceptedVersion = %d, want %d", saved.PolicyAcceptedVersion, Version)
	}
}

func TestAcceptPolicyStaleVersion(t *testing.T) {
	db := openTestDB(t)
	u := models.User{ID: "33333333-3333-3333-3333-333333333333", Login: "p3", ProviderUUID: "33333333-3333-3333-3333-333333333333"}
	if err := db.Create(&u).Error; err != nil {
		t.Fatalf("create user: %v", err)
	}
	app := newTestApp(t, db, &u)

	req := httptest.NewRequest("POST", "/api/policy/accept", strings.NewReader(`{"version":999}`))
	req.Header.Set("Content-Type", "application/json")
	res, err := app.Test(req)
	if err != nil {
		t.Fatalf("app.Test: %v", err)
	}
	if res.StatusCode != 409 {
		t.Fatalf("status = %d, want 409", res.StatusCode)
	}
}

func TestPrivacyPage(t *testing.T) {
	app := newTestApp(t, openTestDB(t), nil)
	res, err := app.Test(httptest.NewRequest("GET", "/privacy", nil))
	if err != nil {
		t.Fatalf("app.Test: %v", err)
	}
	if res.StatusCode != 200 {
		t.Fatalf("status = %d, want 200", res.StatusCode)
	}
	html, _ := io.ReadAll(res.Body)
	if !strings.Contains(string(html), "<h1") || !strings.Contains(string(html), "скриншот") {
		t.Errorf("страница не похожа на отрендеренную политику")
	}
}
```

- [ ] **Step 3: Убедиться, что тест падает**

```bash
$DOCKER_GO go test ./internal/policy/
```
Expected: FAIL — `NewHandler` не определён.

- [ ] **Step 4: Реализовать handler.go**

Создать `backend/internal/policy/handler.go`:

```go
package policy

import (
	"bytes"
	"net/http"
	"sync"

	"launcher-backend/internal/models"

	"github.com/gofiber/fiber/v3"
	"github.com/yuin/goldmark"
	"gorm.io/gorm"
)

// CurrentUserFn достаёт авторизованного пользователя из контекста запроса.
// Инжектится снаружи (auth.CurrentUser), чтобы policy не импортировал auth.
type CurrentUserFn func(c fiber.Ctx) (models.User, bool)

type Handler struct {
	db *gorm.DB
}

func NewHandler(db *gorm.DB) Handler { return Handler{db: db} }

func (h Handler) RegisterRoutes(app *fiber.App, requireAuth fiber.Handler, currentUser CurrentUserFn) {
	app.Get("/api/policy", h.get)
	app.Post("/api/policy/accept", requireAuth, h.accept(currentUser))
	app.Get("/privacy", h.page)
}

func (h Handler) get(c fiber.Ctx) error {
	return c.JSON(fiber.Map{
		"version":   Version,
		"updatedAt": Updated,
		"text":      Text(),
	})
}

type acceptRequest struct {
	Version int `json:"version"`
}

func (h Handler) accept(currentUser CurrentUserFn) fiber.Handler {
	return func(c fiber.Ctx) error {
		user, ok := currentUser(c)
		if !ok {
			return c.Status(http.StatusUnauthorized).JSON(fiber.Map{"message": "Требуется авторизация"})
		}
		var req acceptRequest
		if err := c.Bind().Body(&req); err != nil {
			return c.Status(http.StatusBadRequest).JSON(fiber.Map{"message": "Некорректный запрос"})
		}
		// Клиент принимал устаревшую редакцию — пусть перечитает и примет заново.
		if req.Version != Version {
			return c.Status(http.StatusConflict).JSON(fiber.Map{
				"message": "Версия политики изменилась, ознакомьтесь с новой редакцией",
				"version": Version,
			})
		}
		if err := RecordConsent(c.Context(), h.db, user.ID, SourceLauncher, c.IP()); err != nil {
			return c.Status(http.StatusInternalServerError).JSON(fiber.Map{"message": "Не удалось сохранить согласие"})
		}
		return c.SendStatus(http.StatusNoContent)
	}
}

// Страница /privacy: markdown рендерится в HTML лениво один раз.
var (
	pageOnce sync.Once
	pageHTML []byte
)

func renderPage() []byte {
	var body bytes.Buffer
	if err := goldmark.Convert([]byte(Text()), &body); err != nil {
		body.Reset()
		body.WriteString("<pre>" + Text() + "</pre>")
	}
	var page bytes.Buffer
	page.WriteString(`<!DOCTYPE html><html lang="ru"><head><meta charset="utf-8">` +
		`<meta name="viewport" content="width=device-width, initial-scale=1">` +
		`<title>Политика конфиденциальности</title><style>` +
		`body{max-width:760px;margin:2rem auto;padding:0 1rem;font-family:system-ui,sans-serif;` +
		`line-height:1.6;color:#1a1a1a}h1,h2{line-height:1.3}</style></head><body>`)
	page.Write(body.Bytes())
	page.WriteString(`</body></html>`)
	return page.Bytes()
}

func (h Handler) page(c fiber.Ctx) error {
	pageOnce.Do(func() { pageHTML = renderPage() })
	c.Set(fiber.HeaderContentType, fiber.MIMETextHTMLCharsetUTF8)
	return c.Send(pageHTML)
}
```

- [ ] **Step 5: Тесты зелёные**

```bash
$DOCKER_GO go test ./internal/policy/
```
Expected: PASS.

- [ ] **Step 6: Зарегистрировать роуты в cmd/server**

В `backend/cmd/server/main.go`:
1. В импорты добавить `"launcher-backend/internal/policy"`.
2. После строки `auth.NewHandler(authService).RegisterRoutes(app)` (строка ~107) добавить:

```go
	policy.NewHandler(db).RegisterRoutes(app, authService.RequireAuth(), auth.CurrentUser)
```

- [ ] **Step 7: Полная проверка**

```bash
$DOCKER_GO go vet ./...
$DOCKER_GO go build ./cmd/server
```
Expected: без ошибок.

- [ ] **Step 8: Commit**

```bash
git add backend/internal/policy/ backend/cmd/server/main.go backend/go.mod backend/go.sum
git commit -m "feat(policy): роуты /api/policy, /api/policy/accept и страница /privacy"
```

---

### Task 4: Блок policy в ответе логина

**Files:**
- Modify: `backend/internal/auth/service.go` (структура `LoginResult` строка ~26, функция `Login` строка ~104)

**Interfaces:**
- Consumes: `policy.StatusFor` (Task 2).
- Produces: JSON-ответ `/api/auth/login` получает поле `"policy":{"required":bool,"version":int}` — его читает лаунчер (Task 8).

- [ ] **Step 1: Добавить поле в LoginResult и заполнить**

В `backend/internal/auth/service.go`:
1. В импорты добавить `"launcher-backend/internal/policy"`.
2. В `LoginResult` добавить поле:

```go
type LoginResult struct {
	Token     string        `json:"token"`
	ExpiresAt time.Time     `json:"expiresAt"`
	User      models.User   `json:"user"`
	Message   string        `json:"message"`
	Policy    policy.Status `json:"policy"`
}
```

3. В `Login()` в возвращаемом значении (строка ~104) добавить:

```go
	return LoginResult{
		Token:     token,
		ExpiresAt: expiresAt,
		User:      user,
		Message:   message,
		Policy:    policy.StatusFor(&user),
	}, nil
```

- [ ] **Step 2: Проверка**

```bash
$DOCKER_GO go vet ./...
$DOCKER_GO go test ./internal/auth/
```
Expected: PASS (существующие тесты middleware не задеты). Отдельный тест не нужен: `StatusFor` покрыт в Task 2, здесь — только проброс поля.

- [ ] **Step 3: Commit**

```bash
git add backend/internal/auth/service.go
git commit -m "feat(auth): блок policy (required/version) в ответе логина"
```

---

### Task 5: Enforcement — 451 в anticheat handshake/init

**Files:**
- Modify: `backend/internal/anticheat/handler.go` (функция `init`, после `auth.CurrentUser` строка ~171)
- Test: `backend/internal/anticheat/policy_gate_test.go`

**Interfaces:**
- Consumes: `policy.NeedsConsent` (Task 2).
- Produces: `POST /api/anticheat/handshake/init` → HTTP 451 `{"code":"policy_required","message":...}` для юзера без актуального согласия. Лаунчер обрабатывает в Task 8.

- [ ] **Step 1: Написать падающий тест**

Создать `backend/internal/anticheat/policy_gate_test.go`:

```go
package anticheat

import (
	"net/http/httptest"
	"strings"
	"testing"

	"launcher-backend/internal/models"
	"launcher-backend/internal/policy"

	"github.com/gofiber/fiber/v3"
)

// newPolicyGateApp — init с юзером, подложенным напрямую (ключ Locals тот же,
// что использует auth.RequireAuth). Сервис nil: до него дело не доходит.
func newPolicyGateApp(t *testing.T, acceptedVersion int) *fiber.App {
	t.Helper()
	app := fiber.New()
	inject := func(c fiber.Ctx) error {
		c.Locals("current-user", models.User{
			ID:                    "u-policy",
			Login:                 "player",
			PolicyAcceptedVersion: acceptedVersion,
		})
		return c.Next()
	}
	NewHandler(nil).RegisterRoutes(app, inject)
	return app
}

func postInitRaw(t *testing.T, app *fiber.App, body string) int {
	t.Helper()
	req := httptest.NewRequest("POST", "/api/anticheat/handshake/init", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	res, err := app.Test(req)
	if err != nil {
		t.Fatalf("app.Test: %v", err)
	}
	return res.StatusCode
}

func TestPolicyGateBlocksWithoutConsent(t *testing.T) {
	app := newPolicyGateApp(t, 0)
	if status := postInitRaw(t, app, "{}"); status != 451 {
		t.Fatalf("status = %d, want 451", status)
	}
}

func TestPolicyGatePassesWithConsent(t *testing.T) {
	app := newPolicyGateApp(t, policy.Version)
	// Мусорное тело: гейт пройден, запрос падает на Bind (400) ДО вызова сервиса.
	if status := postInitRaw(t, app, "not-json"); status != 400 {
		t.Fatalf("status = %d, want 400 (гейт пройден, свалились на парсинге)", status)
	}
}
```

- [ ] **Step 2: Убедиться, что тест падает**

```bash
$DOCKER_GO go test ./internal/anticheat/ -run TestPolicyGate
```
Expected: FAIL — `TestPolicyGateBlocksWithoutConsent` получает не 451.

- [ ] **Step 3: Добавить проверку в init**

В `backend/internal/anticheat/handler.go`:
1. В импорты добавить `"launcher-backend/internal/policy"`.
2. В функции `init` сразу после блока получения юзера (`user, ok := auth.CurrentUser(c)` / 401, строка ~171), ПЕРЕД `h.initLimiter.allow`, добавить:

```go
	// Юридический гейт: без принятой актуальной Политики конфиденциальности
	// launch-token не выдаётся. 451 Unavailable For Legal Reasons.
	if policy.NeedsConsent(&user) {
		return c.Status(http.StatusUnavailableForLegalReasons).JSON(fiber.Map{
			"code":    "policy_required",
			"message": "Примите Политику конфиденциальности: выйдите из аккаунта в лаунчере и войдите снова.",
		})
	}
```

- [ ] **Step 4: Тесты зелёные (включая старые)**

```bash
$DOCKER_GO go test ./internal/anticheat/
```
Expected: PASS — и новые TestPolicyGate*, и существующие (version_gate_test не задет: его passthrough не кладёт юзера, а 401 отдаётся до policy-гейта).

- [ ] **Step 5: Commit**

```bash
git add backend/internal/anticheat/
git commit -m "feat(anticheat): 451 policy_required в handshake/init без согласия"
```

---

### Task 6: Бот — гейт согласия и экран политики

**Files:**
- Modify: `backend/internal/bot/menu.go` (константы cb*, новые build-функции, helper policyURL)
- Modify: `backend/internal/bot/callbacks.go` (гейт + case cbPolicyAccept)
- Modify: `backend/internal/bot/lifecycle.go` (гейт в beginPasswordFlow/beginEmailFlow)
- Modify: `backend/internal/bot/totp.go` (гейт в beginTotpFlow)
- Test: `backend/internal/bot/policy_test.go`

**Interfaces:**
- Consumes: `policy.NeedsConsent`, `policy.RecordConsent`, `policy.SourceBot` (Task 2).
- Produces: константа `cbPolicyAccept = "m:policy:ok"`, функции `buildPolicyScreen(privacyURL string) (string, map[string]any)`, `policyGateApplies(v menuView, data string) bool`, `(s *Service) policyURL() string`, `(s *Service) policyGateText(chatID, telegramUID int64) (bool, error)`. Task 7 переиспользует policyURL и паттерн экрана.

- [ ] **Step 1: Написать падающие тесты**

Создать `backend/internal/bot/policy_test.go`:

```go
package bot

import (
	"strings"
	"testing"

	"launcher-backend/internal/models"
	"launcher-backend/internal/policy"
)

func TestPolicyGateApplies(t *testing.T) {
	noConsent := menuView{User: &models.User{PolicyAcceptedVersion: 0}}
	consent := menuView{User: &models.User{PolicyAcceptedVersion: policy.Version}}
	unlinked := menuView{}

	if !policyGateApplies(noConsent, cbProfile) {
		t.Error("привязанный без согласия должен блокироваться")
	}
	if policyGateApplies(noConsent, cbPolicyAccept) {
		t.Error("кнопка принятия не должна блокироваться гейтом")
	}
	if policyGateApplies(consent, cbProfile) {
		t.Error("с актуальным согласием гейт не применяется")
	}
	if policyGateApplies(unlinked, cbProfile) {
		t.Error("непривязанных гейт не трогает (их ловит callbackNeedsLink)")
	}
}

func TestBuildPolicyScreen(t *testing.T) {
	text, markup := buildPolicyScreen("https://example.com/privacy")
	if !strings.Contains(text, "скриншоты экрана") {
		t.Error("выжимка должна упоминать скриншоты экрана")
	}
	raw := markupString(t, markup)
	if !strings.Contains(raw, cbPolicyAccept) {
		t.Errorf("нет кнопки принятия: %s", raw)
	}
	if !strings.Contains(raw, "https://example.com/privacy") {
		t.Errorf("нет URL-кнопки полного текста: %s", raw)
	}
}
```

Если helper `markupString` (сериализация markup в строку для assert'ов) отсутствует в `menu_test.go` — добавить в `policy_test.go`:

```go
import "encoding/json"

func markupString(t *testing.T, markup map[string]any) string {
	t.Helper()
	b, err := json.Marshal(markup)
	if err != nil {
		t.Fatalf("marshal markup: %v", err)
	}
	return string(b)
}
```

(Если в `menu_test.go` уже есть аналогичный helper — использовать его, не дублировать.)

- [ ] **Step 2: Убедиться, что тесты падают**

```bash
$DOCKER_GO go test ./internal/bot/ -run "TestPolicyGateApplies|TestBuildPolicyScreen"
```
Expected: FAIL — функции не определены.

- [ ] **Step 3: Реализовать экран и гейт в menu.go**

В `backend/internal/bot/menu.go`:

1. В блок констант `cb*` добавить:

```go
	cbPolicyAccept    = "m:policy:ok"
	cbPolicyRegAccept = "m:policy:reg"
```

2. В импорты добавить `"launcher-backend/internal/policy"`.

3. После `buildLauncherScreen` добавить:

```go
// buildPolicyScreen — блокирующий экран согласия для привязанного пользователя:
// пока политика не принята, все действия меню ведут сюда.
func buildPolicyScreen(privacyURL string) (string, map[string]any) {
	text := "🔒 <b>Политика конфиденциальности</b>\n\n" +
		"Мы обновили правила работы с данными. Чтобы продолжить пользоваться " +
		"ботом и играть на сервере, примите Политику конфиденциальности.\n\n" +
		"Мы собираем: логин, e-mail, Telegram ID, идентификатор оборудования (HWID), " +
		"IP-адрес, данные игровых сессий и античита, включая <b>скриншоты экрана " +
		"во время игры</b> (по запросу администрации).\n\n" +
		"<i>Полный текст — по кнопке ниже.</i>"
	return text, telegram.InlineMarkup(
		[]telegram.InlineBtn{{Text: "📄 Читать полностью", URL: privacyURL}},
		[]telegram.InlineBtn{{Text: "✅ Принимаю", Data: cbPolicyAccept}},
	)
}

// policyGateApplies — нужно ли вместо запрошенного экрана показать политику.
// Непривязанных (User == nil) не трогаем: их ограничивает callbackNeedsLink.
func policyGateApplies(v menuView, data string) bool {
	if v.User == nil || !policy.NeedsConsent(v.User) {
		return false
	}
	return data != cbPolicyAccept
}

// policyURL — публичная страница полного текста политики.
func (s *Service) policyURL() string {
	return strings.TrimRight(s.Cfg.PublicBaseURL, "/") + "/privacy"
}
```

- [ ] **Step 4: Тесты зелёные**

```bash
$DOCKER_GO go test ./internal/bot/ -run "TestPolicyGateApplies|TestBuildPolicyScreen"
```
Expected: PASS.

- [ ] **Step 5: Врезать гейт и обработчик принятия в callbacks.go**

В `backend/internal/bot/callbacks.go`:

1. В импорты добавить `"launcher-backend/internal/policy"`.

2. В `HandleCallback` после блока `callbackNeedsLink` (строка ~59), перед `switch data`, добавить:

```go
	// Гейт политики: привязанный пользователь без актуального согласия
	// с любой кнопки попадает на экран политики.
	if policyGateApplies(v, data) {
		s.answerCb(cb.ID, "Сначала примите политику конфиденциальности.", false)
		text, markup := buildPolicyScreen(s.policyURL())
		return s.editMenuScreen(chatID, msgID, text, markup)
	}
```

3. В `switch data` перед `default` добавить:

```go
	case cbPolicyAccept:
		if v.User == nil {
			s.answerCb(cb.ID, "Аккаунт не привязан.", true)
			text, markup := buildHomeScreen(v, "")
			return s.editMenuScreen(chatID, msgID, text, markup)
		}
		if err := policy.RecordConsent(s.ctx(), s.DB, v.User.ID, policy.SourceBot, ""); err != nil {
			s.answerCb(cb.ID, "Ошибка, попробуйте ещё раз", false)
			return err
		}
		s.answerCb(cb.ID, "Спасибо!", false)
		nv, err := s.menuViewFor(telegramUID)
		if err != nil {
			return err
		}
		text, markup := buildHomeScreen(nv, "✅ Политика конфиденциальности принята.")
		return s.editMenuScreen(chatID, msgID, text, markup)
```

- [ ] **Step 6: Гейт в текстовых сценариях**

1. В `backend/internal/bot/menu.go` (или рядом с policyGateApplies) добавить метод:

```go
// policyGateText — гейт для текстовых сценариев привязанного пользователя:
// если согласия нет, шлёт экран политики новым меню-сообщением и возвращает true.
func (s *Service) policyGateText(chatID, telegramUID int64) (bool, error) {
	u, err := repo.FindUserByTelegram(s.ctx(), s.DB, telegramUID)
	if err != nil || u == nil {
		return false, err // непривязанных ловят существующие проверки linkedUID
	}
	if !policy.NeedsConsent(u) {
		return false, nil
	}
	text, markup := buildPolicyScreen(s.policyURL())
	if old, err2 := repo.ReadMenuMessage(s.ctx(), s.DB, chatID); err2 == nil && old > 0 {
		_ = telegram.DeleteMessage(s.HTTP, s.Cfg.TelegramBotToken, chatID, old)
	}
	id, err := telegram.SendMessageHTMLWithID(s.HTTP, s.Cfg.TelegramBotToken, chatID, text, markup, s.bannerPreview())
	if err != nil {
		return true, err
	}
	return true, repo.SaveMenuMessage(s.ctx(), s.DB, chatID, id)
}
```

2. В `backend/internal/bot/lifecycle.go` в `beginPasswordFlow` и `beginEmailFlow` сразу после проверки `linkedUID`/`forbidNotLinked` добавить:

```go
	if blocked, err := s.policyGateText(chatID, telegramUID); err != nil || blocked {
		return err
	}
```

3. В `backend/internal/bot/totp.go` найти `beginTotpFlow` и добавить ту же вставку после аналогичной проверки привязки.

- [ ] **Step 7: Полная проверка пакета**

```bash
$DOCKER_GO go vet ./...
$DOCKER_GO go test ./internal/bot/
```
Expected: PASS.

- [ ] **Step 8: Commit**

```bash
git add backend/internal/bot/
git commit -m "feat(bot): гейт политики конфиденциальности + экран принятия"
```

---

### Task 7: Бот — согласие при регистрации

**Files:**
- Modify: `backend/internal/bot/register.go` (`beginRegisterFlow` строка ~48, `handleRegOTP` строка ~163)
- Modify: `backend/internal/bot/menu.go` (новая build-функция)
- Modify: `backend/internal/bot/callbacks.go` (case cbPolicyRegAccept)
- Test: `backend/internal/bot/policy_test.go` (дополнить)

**Interfaces:**
- Consumes: `cbPolicyRegAccept`, `policyURL()` (Task 6); `policy.RecordConsent` (Task 2); `repo.RegisterNewUser` (существующий).
- Produces: `buildRegPolicyScreen(privacyURL string) (string, map[string]any)`; `(s *Service) startRegisterSteps(chatID int64, sender *tele.User) error` (бывшее тело beginRegisterFlow).

- [ ] **Step 1: Дополнить тест**

В `backend/internal/bot/policy_test.go` добавить:

```go
func TestBuildRegPolicyScreen(t *testing.T) {
	text, markup := buildRegPolicyScreen("https://example.com/privacy")
	if !strings.Contains(text, "скриншоты экрана") {
		t.Error("выжимка должна упоминать скриншоты экрана")
	}
	raw := markupString(t, markup)
	if !strings.Contains(raw, cbPolicyRegAccept) {
		t.Errorf("нет кнопки принятия для регистрации: %s", raw)
	}
	if !strings.Contains(raw, cbHome) {
		t.Errorf("нет кнопки «Назад»: %s", raw)
	}
}
```

- [ ] **Step 2: Убедиться, что тест падает**

```bash
$DOCKER_GO go test ./internal/bot/ -run TestBuildRegPolicyScreen
```
Expected: FAIL — `buildRegPolicyScreen` не определена.

- [ ] **Step 3: Экран согласия для регистрации**

В `backend/internal/bot/menu.go` после `buildPolicyScreen` добавить:

```go
// buildRegPolicyScreen — шаг согласия перед регистрацией нового аккаунта.
// «Назад» ведёт на главный экран: без принятия регистрация не стартует.
func buildRegPolicyScreen(privacyURL string) (string, map[string]any) {
	text := "🔒 <b>Политика конфиденциальности</b>\n\n" +
		"Для создания аккаунта примите Политику конфиденциальности.\n\n" +
		"Мы собираем: логин, e-mail, Telegram ID, идентификатор оборудования (HWID), " +
		"IP-адрес, данные игровых сессий и античита, включая <b>скриншоты экрана " +
		"во время игры</b> (по запросу администрации).\n\n" +
		"<i>Полный текст — по кнопке ниже.</i>"
	return text, telegram.InlineMarkup(
		[]telegram.InlineBtn{{Text: "📄 Читать полностью", URL: privacyURL}},
		[]telegram.InlineBtn{{Text: "✅ Принимаю и продолжаю", Data: cbPolicyRegAccept}},
		backRow(),
	)
}
```

- [ ] **Step 4: Разбить beginRegisterFlow**

В `backend/internal/bot/register.go` заменить `beginRegisterFlow` (строки 48–61) на:

```go
// beginRegisterFlow — вход в регистрацию: сперва обязательный шаг согласия
// с политикой. Сами шаги регистрации стартуют из cbPolicyRegAccept.
func (s *Service) beginRegisterFlow(chatID int64, sender *tele.User) error {
	if ok, err := s.alreadyLinkedChat(chatID, sender); err != nil || ok {
		return err
	}
	text, markup := buildRegPolicyScreen(s.policyURL())
	return s.notifyHTML(chatID, text, markup)
}

// startRegisterSteps — шаги регистрации после принятия политики.
func (s *Service) startRegisterSteps(chatID int64, sender *tele.User) error {
	if ok, err := s.alreadyLinkedChat(chatID, sender); err != nil || ok {
		return err
	}
	ep := repo.EmptyPayload()
	if err := repo.SaveDialogue(s.ctx(), s.DB, chatID, repo.FlowRegUsername, &ep); err != nil {
		return err
	}
	return s.notifyHTML(chatID, s.msgWithCancelHint(
		"<b>Регистрация, шаг 1</b>\n\n"+
			"Придумайте <b>логин</b> (в игре и на сайте будет тот же ник, пока админ не сменит): латиница, цифры или символ <code>_</code>, от 3 до 32 символов.\n\n"+
			"<i>Пример: <code>my_player_01</code></i>"),
		homeReplyKeyboardMarkup())
}
```

- [ ] **Step 5: Обработчик кнопки в callbacks.go**

В `switch data` рядом с `case cbPolicyAccept` добавить:

```go
	case cbPolicyRegAccept:
		s.answerCb(cb.ID, "", false)
		return s.startRegisterSteps(chatID, c.Sender())
```

- [ ] **Step 6: Зафиксировать согласие при создании аккаунта**

В `backend/internal/bot/register.go`:
1. В импорты добавить `"launcher-backend/internal/policy"`.
2. В `handleRegOTP` после успешного `repo.RegisterNewUser` и `repo.BindTelegram` (строки ~185–196) добавить:

```go
	// Пользователь дошёл сюда только через кнопку «Принимаю» (beginRegisterFlow
	// стартует шаги лишь из cbPolicyRegAccept) — фиксируем согласие аккаунту.
	if err := policy.RecordConsent(s.ctx(), s.DB, uid, policy.SourceBot, ""); err != nil && s.Log != nil {
		s.Log.Warn("policy consent при регистрации", "err", err)
	}
```

- [ ] **Step 7: Тесты зелёные**

```bash
$DOCKER_GO go vet ./...
$DOCKER_GO go test ./internal/bot/
```
Expected: PASS.

- [ ] **Step 8: Commit**

```bash
git add backend/internal/bot/
git commit -m "feat(bot): шаг согласия с политикой перед регистрацией"
```

---

### Task 8: Лаунчер — клиент политики, bootstrap и обработка 451

**Files:**
- Modify: `launcher-slint/src/main.rs` (структуры ~217–232, `bootstrap_session` ~899, `apply_session` ~921, новые функции рядом с `login_to_backend` ~1154)
- Modify: `launcher-slint/src/anticheat/handshake.rs` (enum `InitOutcome` ~40, `init` ~103)
- Modify: `launcher-slint/src/anticheat/mod.rs` (маппинг исхода ~62)

**Interfaces:**
- Consumes: `GET /api/policy`, `POST /api/policy/accept` (Task 3), поле `policyAcceptedVersion` в user JSON (Task 1), 451 от init (Task 5).
- Produces (использует Task 9): `struct PolicyInfo {version: i32, text: String}`, `fn fetch_policy(config) -> Result<PolicyInfo, String>`, `fn accept_policy(config, token, version) -> Result<(), String>`, поле `SessionData.policy: Option<PolicyInfo>`, `pub const POLICY_REQUIRED_ERR: &str` в `anticheat::handshake`, вариант `InitOutcome::PolicyRequired`.

- [ ] **Step 1: Структуры и HTTP-функции**

В `launcher-slint/src/main.rs`:

1. В `AuthUser` (строка ~226) добавить поле:

```rust
    #[serde(default)]
    policy_accepted_version: i32,
```

2. Рядом с `LoginResponse` добавить:

```rust
#[derive(Debug, Deserialize, Clone)]
#[serde(rename_all = "camelCase")]
struct PolicyInfo {
    version: i32,
    text: String,
}
```

3. Рядом с `fetch_profiles` (строка ~1208) добавить:

```rust
// Текст и версия Политики конфиденциальности (публичный эндпоинт, без auth).
fn fetch_policy(config: &AppConfig) -> Result<PolicyInfo, String> {
    let client = http_client()?;
    let response = client
        .get(format!("{}/api/policy", config.api_url.trim_end_matches('/')))
        .send()
        .map_err(|_| "Backend лаунчера недоступен.".to_string())?;
    parse_json_response(response, "Backend вернул некорректную политику")
}

// Фиксирует согласие на сервере. 409 = версия успела смениться.
fn accept_policy(config: &AppConfig, token: &str, version: i32) -> Result<(), String> {
    let client = http_client()?;
    let response = client
        .post(format!(
            "{}/api/policy/accept",
            config.api_url.trim_end_matches('/')
        ))
        .bearer_auth(token)
        .json(&serde_json::json!({ "version": version }))
        .send()
        .map_err(|_| "Backend лаунчера недоступен.".to_string())?;
    if response.status().is_success() {
        return Ok(());
    }
    Err("Не удалось сохранить согласие. Попробуйте ещё раз.".to_string())
}
```

- [ ] **Step 2: Политика в bootstrap_session**

1. В структуру `SessionData` (найти по `struct SessionData`) добавить поле:

```rust
    policy: Option<PolicyInfo>,
```

2. В `bootstrap_session` (строка ~899) перед `Ok(SessionData {...})` добавить и включить поле:

```rust
    // Политика конфиденциальности: если пользователь не принимал текущую
    // версию — экран согласия. Сбой запроса = fail-open на клиенте
    // (сервер всё равно не выдаст launch-token без согласия).
    let policy = match fetch_policy(config) {
        Ok(p) if p.version > user.policy_accepted_version => Some(p),
        _ => None,
    };
    Ok(SessionData {
        token,
        user,
        expires_at,
        message,
        profiles,
        selected_profile_id,
        news,
        policy,
    })
```

- [ ] **Step 3: 451 в anticheat handshake**

В `launcher-slint/src/anticheat/handshake.rs`:

1. В enum `InitOutcome` (строка ~40) добавить вариант:

```rust
    /// 451: пользователь не принял актуальную Политику конфиденциальности.
    PolicyRequired,
```

2. Там же объявить сентинел (после enum):

```rust
/// Сентинел-ошибка: по ней main показывает экран политики вместо текста.
pub const POLICY_REQUIRED_ERR: &str = "__policy_required__";
```

3. В `init()` после блока 426 (строка ~117) добавить:

```rust
    // 451 = не принята Политика конфиденциальности: launch-token не выдан.
    if status.as_u16() == 451 {
        return InitOutcome::PolicyRequired;
    }
```

4. В `launcher-slint/src/anticheat/mod.rs` в match по `InitOutcome` (строка ~62, рядом с `Blocked`/`UpdateRequired`) добавить:

```rust
            handshake::InitOutcome::PolicyRequired => Err(handshake::POLICY_REQUIRED_ERR.to_string()),
```

- [ ] **Step 4: Компиляция**

```bash
cd launcher-slint && cargo build 2>&1 | tail -20
```
Expected: ошибки только про неиспользуемые `fetch_policy`/`accept_policy`/`SessionData.policy` отсутствуют (Rust не ругается на неиспользуемые поля структур с derive; функции могут дать warning `dead_code` — это ок до Task 9). Сборка успешна.

- [ ] **Step 5: Commit**

```bash
git add launcher-slint/src/
git commit -m "feat(launcher): клиент политики, policy в bootstrap, 451 из handshake"
```

---

### Task 9: Лаунчер — Slint-экран политики и обвязка

**Files:**
- Modify: `launcher-slint/ui/app.slint` (свойства ~395–446, оверлей в конец корневого компонента после TOTP-оверлея ~1702)
- Modify: `launcher-slint/src/main.rs` (`apply_session` ~921, обвязка колбэков рядом с `on_login_requested` ~499, обработчик logout ~556, обработка ошибки запуска)

**Interfaces:**
- Consumes: всё из Task 8.
- Produces: свойства `policy-visible`, `policy-text`, `policy-version-label`, `policy-version`, колбэк `policy-accept-requested()` в `AppWindow`.

- [ ] **Step 1: Свойства и колбэк в app.slint**

В корневом компоненте `AppWindow` рядом с `in property <bool> is-authenticated;` (строка ~395) добавить:

```slint
    in property <bool> policy-visible;
    in property <string> policy-text;
    in property <string> policy-version-label;
    in property <int> policy-version;
```

Рядом с `callback logout-requested();` (строка ~432) добавить:

```slint
    callback policy-accept-requested();
```

- [ ] **Step 2: Оверлей политики**

В конец корневого компонента (после TOTP-оверлея, строка ~1702+, последним элементом — чтобы был поверх всего) добавить. Вёрстка строго абсолютная (Slint-гоча):

```slint
    // === Оверлей Политики конфиденциальности (блокирует всё до принятия) ===
    Rectangle {
        x: 0; y: 0;
        width: parent.width; height: parent.height;
        background: #0d0f12;
        visible: root.policy-visible;

        // Глушим клики по подлежащему UI.
        TouchArea {}

        Text {
            x: 48px; y: 32px;
            width: parent.width - 96px;
            text: "Политика конфиденциальности";
            color: #e0e2e5;
            font-size: 24px;
            font-weight: 800;
            overflow: elide;
        }
        Text {
            x: 48px; y: 66px;
            width: parent.width - 96px;
            text: root.policy-version-label;
            color: #8a8f98;
            font-size: 13px;
            overflow: elide;
        }

        Rectangle {
            x: 48px; y: 92px;
            width: parent.width - 96px;
            height: parent.height - 92px - 118px;
            background: #14171c;
            border-radius: 10px;
            clip: true;

            Flickable {
                x: 16px; y: 12px;
                width: parent.width - 32px;
                height: parent.height - 24px;
                viewport-height: policy-body.preferred-height + 24px;

                policy-body := Text {
                    x: 0; y: 0;
                    width: parent.width - 12px;
                    text: root.policy-text;
                    color: #c6cad1;
                    font-size: 14px;
                    wrap: word-wrap;
                }
            }
        }

        Text {
            x: 48px; y: parent.height - 106px;
            width: parent.width - 96px;
            text: "Нажимая «Принять и продолжить», вы подтверждаете согласие с условиями.";
            color: #8a8f98;
            font-size: 12px;
            overflow: elide;
        }

        accept-btn := Rectangle {
            x: 48px; y: parent.height - 76px;
            width: 250px; height: 42px;
            border-radius: 8px;
            background: accept-ta.has-hover ? #d8b45a : #c9a44a;

            Text {
                x: 0; y: 0;
                width: parent.width; height: parent.height;
                text: "Принять и продолжить";
                color: #171310;
                font-size: 15px;
                font-weight: 700;
                horizontal-alignment: center;
                vertical-alignment: center;
            }
            accept-ta := TouchArea {
                clicked => { root.policy-accept-requested(); }
            }
        }

        exit-btn := Rectangle {
            x: 48px + 266px; y: parent.height - 76px;
            width: 120px; height: 42px;
            border-radius: 8px;
            border-width: 1px;
            border-color: #3a3f47;

            Text {
                x: 0; y: 0;
                width: parent.width; height: parent.height;
                text: "Выйти";
                color: #c6cad1;
                font-size: 15px;
                horizontal-alignment: center;
                vertical-alignment: center;
            }
            TouchArea {
                clicked => { root.logout-requested(); }
            }
        }

        browser-link := Text {
            x: parent.width - 48px - 220px; y: parent.height - 66px;
            width: 220px;
            text: "Открыть в браузере ↗";
            color: #8fa3c0;
            font-size: 13px;
            horizontal-alignment: right;

            TouchArea {
                clicked => { root.open-url(root.api-url + "/privacy"); }
            }
        }
    }
```

Примечание: цвета подобраны под тёмную палитру существующего UI — при вёрстке свериться с соседними экранами (`#e0e2e5` для заголовков уже используется) и при необходимости взять точные токены из файла. Если у существующей золотой кнопки (`GoldButton`) подходящий API — можно использовать её вместо самодельного `accept-btn`.

- [ ] **Step 3: Обвязка в main.rs**

1. В `apply_session` (строка ~921) после `app.set_message(...)` добавить:

```rust
    match &session.policy {
        Some(p) => {
            app.set_policy_text(p.text.clone().into());
            app.set_policy_version_label(format!("Версия {}", p.version).into());
            app.set_policy_version(p.version);
            app.set_policy_visible(true);
        }
        None => app.set_policy_visible(false),
    }
```

2. Рядом с обвязкой `on_login_requested` (строка ~499) добавить обработчик принятия (`config` и `state` клонируются так же, как в соседних обработчиках):

```rust
    {
        let app_weak = app.as_weak();
        let state = state.clone();
        let config = config.clone();
        app.on_policy_accept_requested(move || {
            let Some(app) = app_weak.upgrade() else { return };
            let token = state
                .lock()
                .ok()
                .map(|s| s.token.clone())
                .unwrap_or_default();
            let version = app.get_policy_version();
            app.set_message("Сохраняю согласие…".into());
            let config = config.clone();
            let app_weak = app_weak.clone();
            thread::spawn(move || {
                let result = accept_policy(&config, &token, version);
                let refreshed = if result.is_err() {
                    // Возможен 409: версия сменилась — перечитываем текст.
                    fetch_policy(&config).ok()
                } else {
                    None
                };
                let _ = slint::invoke_from_event_loop(move || {
                    let Some(app) = app_weak.upgrade() else { return };
                    match result {
                        Ok(()) => {
                            app.set_policy_visible(false);
                            app.set_message("Политика принята. Приятной игры!".into());
                        }
                        Err(message) => {
                            if let Some(p) = refreshed {
                                app.set_policy_text(p.text.into());
                                app.set_policy_version_label(format!("Версия {}", p.version).into());
                                app.set_policy_version(p.version);
                            }
                            app.set_message(message.into());
                        }
                    }
                });
            });
        });
    }
```

3. В обработчике `on_logout_requested` (строка ~556) добавить сброс оверлея:

```rust
        app.set_policy_visible(false);
```
(внутри существующего замыкания, там где сбрасываются остальные свойства UI; если сброс идёт через `invoke_from_event_loop` — добавить туда).

4. Обработка 451 при запуске игры: найти место, где ошибка `LaunchGuard`/handshake попадает в `app.set_message(...)` (путь ошибки запуска, грепнуть `anticheat::LaunchGuard::new` или место, где `Err(reason)` из init показывается игроку), и обернуть:

```rust
    if message == anticheat::handshake::POLICY_REQUIRED_ERR {
        // Редкий случай (окно раскатки): сервер требует согласие — показываем
        // экран политики вместо сырого текста ошибки.
        let config_bg = config.clone();
        let app_weak_bg = app_weak.clone();
        thread::spawn(move || {
            let policy = fetch_policy(&config_bg);
            let _ = slint::invoke_from_event_loop(move || {
                let Some(app) = app_weak_bg.upgrade() else { return };
                if let Ok(p) = policy {
                    app.set_policy_text(p.text.into());
                    app.set_policy_version_label(format!("Версия {}", p.version).into());
                    app.set_policy_version(p.version);
                }
                app.set_policy_visible(true);
            });
        });
    } else {
        app.set_message(message.into());
    }
```
(имена переменных подогнать под контекст места врезки; `handshake` может потребовать `pub` реэкспорт `POLICY_REQUIRED_ERR` из `anticheat::mod` — тогда добавить `pub use handshake::POLICY_REQUIRED_ERR;` в `src/anticheat/mod.rs`).

- [ ] **Step 4: Компиляция и smoke-тест UI**

```bash
cd launcher-slint && cargo build 2>&1 | tail -20
```
Expected: сборка успешна, без warning'ов о dead_code для policy-функций.

Ручной smoke (дев-бэкенд должен отдавать policy): запустить `./dev.sh` в одном терминале, затем:
```bash
npm run dev:launcher
```
Войти тестовым аккаунтом (без согласия в БД) → появляется экран политики со скроллом → «Принять и продолжить» → список профилей. Перезапустить лаунчер → экран не показывается.

- [ ] **Step 5: Commit**

```bash
git add launcher-slint/
git commit -m "feat(launcher): экран Политики конфиденциальности с обязательным принятием"
```

---

### Task 10: Финальная проверка, бамп версии, раскатка

**Files:**
- Modify: `launcher-slint/Cargo.toml` (`version = "0.3.2"` → `"0.3.3"`)
- Modify: `launcher-slint/Cargo.lock` (через `cargo update -p launcher-slint`)
- Modify: `CLAUDE.md` (актуализировать текущую версию лаунчера и упомянуть policy-гейт)

- [ ] **Step 1: Полный прогон бэкенд-тестов и дашборда**

```bash
$DOCKER_GO go vet ./...
$DOCKER_GO go test ./...
cd dashboard && npm run build
```
Expected: всё PASS (дашборд не менялся — только тайпчек).

- [ ] **Step 2: Бамп версии лаунчера**

В `launcher-slint/Cargo.toml`: `version = "0.3.3"`. Затем ОБЯЗАТЕЛЬНО:

```bash
cd launcher-slint && cargo update -p launcher-slint && cargo build
```

- [ ] **Step 3: Актуализировать CLAUDE.md**

В разделе «Релизы лаунчера» сменить текущую версию на 0.3.3; в разделе «Архитектура бэкенда» добавить строку о пакете `policy` (роуты `/api/policy`, `/privacy`, гейт 451 в anticheat init).

- [ ] **Step 4: Commit**

```bash
git add launcher-slint/Cargo.toml launcher-slint/Cargo.lock CLAUDE.md
git commit -m "chore(release): лаунчер 0.3.3 — политика конфиденциальности"
```

- [ ] **Step 5: Раскатка (ручные шаги владельца — порядок важен)**

1. `./deploy.sh` — бэкенд уезжает на srv-129; enforcement 451 активен с этого момента.
2. Собрать плеер-лаунчер: `scripts/prod/build-player-launcher.sh --api-url https://launcher.likonchik.xyz`
3. Залить 0.3.3 через дашборд (раздел «Релизы») сразу с флагом **mandatory** — старые 0.3.2 получат 426 и автообновятся; после обновления увидят экран политики.
4. Проверить: `https://launcher.likonchik.xyz/privacy` открывается; вход в лаунчер показывает политику; бот по любой кнопке требует согласие; после принятия игра запускается.

Окно между шагами 1 и 3, когда 0.3.2 получает «сырое» 451 без экрана, — держать минимальным (минуты).
