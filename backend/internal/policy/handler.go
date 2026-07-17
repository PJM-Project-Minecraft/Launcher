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
	app.Get("/rules", h.rulesPage)
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

// Страницы /privacy и /rules: markdown рендерится в HTML лениво один раз.
var (
	pageOnce  sync.Once
	pageHTML  []byte
	rulesOnce sync.Once
	rulesHTML []byte
)

// pageCSS — брендовое оформление страницы политики. Монохром-воксель, тот же
// визуальный язык, что у витрины /download (палитра из логотипа PJM). Документ
// длинный, поэтому упор на читаемость; акценты — брендовая шапка и воксель-
// маркеры у заголовков секций.
const pageCSS = `
:root{--void:#0b0b0d;--stone:#151519;--side:#33333b;--face:#ededf0;--dim:#8e8e98;--ink:#000}
*{box-sizing:border-box}
html{-webkit-text-size-adjust:100%}
body{
  margin:0;background:var(--void);color:#d3d3d9;
  font-family:"Inter",ui-sans-serif,system-ui,-apple-system,"Segoe UI",sans-serif;
  line-height:1.72;padding:clamp(1.5rem,5vw,3.5rem) 1.25rem 3rem;
  background-image:
    linear-gradient(rgba(255,255,255,.014) 1px,transparent 1px),
    linear-gradient(90deg,rgba(255,255,255,.014) 1px,transparent 1px);
  background-size:34px 34px;background-position:center top;
}
.wrap{max-width:720px;margin:0 auto}
.top{text-align:center;margin-bottom:clamp(1.8rem,5vw,2.8rem)}
.top .logo{
  width:clamp(96px,26vw,132px);height:auto;display:block;margin:0 auto .7rem;
  image-rendering:pixelated;filter:drop-shadow(0 5px 0 rgba(0,0,0,.5));
}
.top .kicker{
  font-family:"Silkscreen",ui-monospace,monospace;font-size:.66rem;
  letter-spacing:.22em;text-transform:uppercase;color:var(--dim);
}
.top .back{
  display:inline-block;margin-bottom:1.3rem;color:var(--dim);text-decoration:none;
  font-size:.82rem;border:2px solid var(--side);padding:.35rem .7rem;
  transition:color .12s,border-color .12s,background .12s;
}
.top .back:hover{color:var(--face);border-color:var(--face);background:var(--stone)}
.doc h1{
  font-size:clamp(1.7rem,6vw,2.4rem);line-height:1.15;font-weight:800;
  letter-spacing:-.02em;color:var(--face);margin:.2rem 0 1rem;text-align:center;
}
/* Строка версии/даты сразу после H1 — пиксельный бейдж по центру */
.doc h1 + p{
  text-align:center;margin:0 0 2.4rem;
}
.doc h1 + p strong{
  display:inline-block;font-family:"Silkscreen",ui-monospace,monospace;
  font-weight:400;font-size:.72rem;letter-spacing:.04em;color:var(--face);
  background:var(--stone);border:2px solid var(--ink);box-shadow:3px 3px 0 var(--ink);
  padding:.45rem .7rem;line-height:1.35;
}
.doc h2{
  font-size:1.16rem;font-weight:700;color:var(--face);line-height:1.3;
  margin:2.4rem 0 .8rem;padding-left:.85rem;position:relative;
}
/* Воксель-маркер секции: мини-блок с обводкой, как грань логотипа */
.doc h2::before{
  content:"";position:absolute;left:0;top:.18em;width:.42rem;height:.9rem;
  background:var(--face);border:2px solid var(--ink);
}
.doc p{margin:.85rem 0}
.doc strong{color:var(--face);font-weight:600}
.doc ul{list-style:none;margin:.85rem 0;padding:0}
.doc li{position:relative;padding-left:1.5rem;margin:.5rem 0}
.doc li::before{
  content:"";position:absolute;left:.15rem;top:.55em;width:.42rem;height:.42rem;
  background:var(--side);border:2px solid var(--ink);
}
.doc a{color:var(--face);text-decoration:underline;text-underline-offset:2px}
.doc a:hover{color:#fff}
footer{
  margin-top:3rem;padding-top:1.5rem;border-top:2px solid var(--stone);
  text-align:center;font-size:.82rem;color:var(--dim);
}
footer a{color:var(--dim);text-decoration:underline;text-underline-offset:3px}
footer a:hover{color:var(--face)}
`

func renderPage(title, markdown string) []byte {
	var body bytes.Buffer
	if err := goldmark.Convert([]byte(markdown), &body); err != nil {
		body.Reset()
		body.WriteString("<pre>" + html.EscapeString(markdown) + "</pre>")
	}
	var page bytes.Buffer
	page.WriteString(`<!DOCTYPE html><html lang="ru"><head><meta charset="utf-8">` +
		`<meta name="viewport" content="width=device-width, initial-scale=1">` +
		`<title>` + html.EscapeString(title) + ` — Project Minecraft</title>` +
		`<meta name="robots" content="noindex">` +
		`<link rel="preconnect" href="https://fonts.googleapis.com">` +
		`<link rel="preconnect" href="https://fonts.gstatic.com" crossorigin>` +
		`<link href="https://fonts.googleapis.com/css2?family=Inter:wght@400;600;700;800&family=Silkscreen:wght@400;700&display=swap" rel="stylesheet">` +
		`<style>` + pageCSS + `</style></head><body><main class="wrap">`)
	page.WriteString(`<header class="top">` +
		`<a class="back" href="/download">← Скачать лаунчер</a>` +
		`<img class="logo" src="/download/pjm.png" width="264" height="264" alt="Project Minecraft">` +
		`<div class="kicker">Project Minecraft</div>` +
		`</header><article class="doc">`)
	page.Write(body.Bytes())
	page.WriteString(`</article>` +
		`<footer>Project Minecraft · <a href="/download">Скачать лаунчер</a></footer>` +
		`</main></body></html>`)
	return page.Bytes()
}

func (h Handler) page(c fiber.Ctx) error {
	pageOnce.Do(func() { pageHTML = renderPage("Политика конфиденциальности", Text()) })
	c.Set(fiber.HeaderContentType, fiber.MIMETextHTMLCharsetUTF8)
	return c.Send(pageHTML)
}

func (h Handler) rulesPage(c fiber.Ctx) error {
	rulesOnce.Do(func() { rulesHTML = renderPage("Правила сервера", RulesText()) })
	c.Set(fiber.HeaderContentType, fiber.MIMETextHTMLCharsetUTF8)
	return c.Send(rulesHTML)
}
