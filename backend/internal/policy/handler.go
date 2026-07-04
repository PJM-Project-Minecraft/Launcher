package policy

import (
	"bytes"
	"errors"
	"html"
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
			if errors.Is(err, gorm.ErrRecordNotFound) {
				return c.Status(http.StatusNotFound).JSON(fiber.Map{"message": "Пользователь не найден"})
			}
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
		body.WriteString("<pre>" + html.EscapeString(Text()) + "</pre>")
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
