package launcherrelease

import (
	_ "embed"
	"errors"
	"fmt"
	"html"
	"strings"

	"github.com/gofiber/fiber/v3"
	"gorm.io/gorm"
)

// Публичная страница скачивания лаунчера (GET /download). Витрина для игроков:
// логотип PJM, версия, кнопки под платформы. Кнопки ведут на публичный роут
// /api/launcher/download/<version>/<platform>. Дизайн выведен из воксельного
// логотипа: монохром (уголь/камень/белый), 3D-блоки-кнопки с изо-экструзией.

//go:embed assets/pjm-logo.png
var logoPNG []byte

// platformCard — данные одной кнопки-платформы для страницы.
type platformCard struct {
	Platform    string // linux-x64 / windows-x64
	Label       string // Linux / Windows
	Icon        string // эмодзи-глиф
	Version     string
	Size        int64
	DownloadURL string
	Primary     bool // подсвеченная (совпала с ОС посетителя)
}

// downloadPage — рендер витрины скачивания. Версии/размеры берутся из последнего
// активного релиза под каждую платформу; отсутствующие платформы просто не
// показываются. Порядок и подсветка зависят от User-Agent посетителя.
func (h Handler) downloadPage(c fiber.Ctx) error {
	primary := detectPlatform(c.Get(fiber.HeaderUserAgent))
	cards := make([]platformCard, 0, len(AllowedPlatforms))
	for _, platform := range AllowedPlatforms {
		release, file, _, err := h.service.LatestFile(c.Context(), platform)
		if err != nil {
			if !errors.Is(err, gorm.ErrRecordNotFound) && logWarn != nil {
				logWarn("download page latest", "platform", platform, "err", err)
			}
			continue
		}
		cards = append(cards, platformCard{
			Platform:    platform,
			Label:       platformLabel(platform),
			Icon:        platformIcon(platform),
			Version:     release.Version,
			Size:        file.Size,
			DownloadURL: "/api/launcher/download/" + release.Version + "/" + platform,
			Primary:     platform == primary,
		})
	}
	// Подсвеченную платформу — первой.
	for i := range cards {
		if cards[i].Primary && i != 0 {
			cards[0], cards[i] = cards[i], cards[0]
			break
		}
	}
	c.Set(fiber.HeaderContentType, fiber.MIMETextHTMLCharsetUTF8)
	return c.SendString(renderDownloadPage(cards, primary != ""))
}

// logo отдаёт встроенный PNG-логотип для страницы (кэшируется браузером надолго —
// файл иммутабельный, при смене логотипа меняется бинарник).
func (h Handler) logo(c fiber.Ctx) error {
	c.Set(fiber.HeaderContentType, "image/png")
	c.Set(fiber.HeaderCacheControl, "public, max-age=86400")
	return c.Send(logoPNG)
}

// logWarn — необязательный логгер предупреждений страницы (инжектится из main
// через SetLogger, чтобы пакет не завязывался на конкретный slog-инстанс). nil — тихо.
var logWarn func(msg string, args ...any)

// SetLogger подключает логгер предупреждений (напр. slog.Warn).
func SetLogger(fn func(msg string, args ...any)) { logWarn = fn }

func platformLabel(platform string) string {
	switch platform {
	case "windows-x64":
		return "Windows"
	case "linux-x64":
		return "Linux"
	}
	return platform
}

func platformIcon(platform string) string {
	switch platform {
	case "windows-x64":
		return "🪟"
	case "linux-x64":
		return "🐧"
	}
	return "⬇"
}

// detectPlatform угадывает ОС посетителя по User-Agent. Возвращает код платформы
// из AllowedPlatforms или "" — тогда обе кнопки показываются равнозначно.
// Android содержит "Linux", но лаунчер десктопный — Android не выделяем.
func detectPlatform(ua string) string {
	u := strings.ToLower(ua)
	if strings.Contains(u, "windows") {
		return "windows-x64"
	}
	if strings.Contains(u, "android") {
		return ""
	}
	if strings.Contains(u, "linux") || strings.Contains(u, "x11") {
		return "linux-x64"
	}
	return ""
}

// humanSize форматирует размер в МБ (десятичные, как в файловых менеджерах).
func humanSize(n int64) string {
	if n <= 0 {
		return ""
	}
	mb := float64(n) / 1_000_000
	if mb < 10 {
		return fmt.Sprintf("%.1f МБ", mb)
	}
	return fmt.Sprintf("%.0f МБ", mb)
}

const downloadPageCSS = `
:root{
  --void:#0b0b0d; --stone:#151519; --stone-hi:#1c1c22;
  --side:#33333b; --face:#ededf0; --dim:#8e8e98; --ink:#000;
}
*{box-sizing:border-box}
html{-webkit-text-size-adjust:100%}
body{
  margin:0; min-height:100dvh; background:var(--void); color:var(--face);
  font-family:"Inter",ui-sans-serif,system-ui,-apple-system,"Segoe UI",sans-serif;
  line-height:1.55; display:flex; flex-direction:column; align-items:center;
  padding:clamp(1.5rem,5vw,4rem) 1.25rem 2.5rem;
  /* Едва заметная блочная сетка — камень под ногами, не декор поверх */
  background-image:
    linear-gradient(rgba(255,255,255,.014) 1px,transparent 1px),
    linear-gradient(90deg,rgba(255,255,255,.014) 1px,transparent 1px);
  background-size:34px 34px; background-position:center top;
}
.wrap{width:100%; max-width:520px; margin:auto 0}
.hero{text-align:center; margin-bottom:clamp(2rem,6vw,3rem)}
.logo{
  width:clamp(148px,44vw,208px); height:auto; display:block; margin:0 auto .9rem;
  image-rendering:pixelated;
  filter:drop-shadow(0 6px 0 rgba(0,0,0,.55));
  animation:rise .5s cubic-bezier(.2,.7,.3,1) both;
}
.kicker{
  font-family:"Silkscreen",ui-monospace,monospace; font-size:.72rem;
  letter-spacing:.22em; text-transform:uppercase; color:var(--dim);
  margin:.2rem 0 .55rem;
}
h1{
  font-size:clamp(2.1rem,9vw,3rem); line-height:1; margin:0 0 .85rem;
  font-weight:800; letter-spacing:-.02em;
}
.ver{
  display:inline-flex; align-items:center; gap:.5rem;
  font-family:"Silkscreen",ui-monospace,monospace; font-size:.8rem;
  padding:.4rem .7rem; color:var(--face);
  background:var(--stone); border:2px solid var(--ink);
  box-shadow:3px 3px 0 var(--ink);
}
.ver .must{color:#000; background:var(--face); padding:.12rem .4rem; font-size:.66rem}
.cards{display:flex; flex-direction:column; gap:1.15rem}
/* Signature: воксель-блок. Ступенчатая тень = изо-экструзия логотипа. */
.card{
  display:flex; align-items:center; gap:1rem; width:100%; text-decoration:none;
  color:var(--face); text-align:left; cursor:pointer;
  padding:1.05rem 1.25rem; background:var(--stone);
  border:2px solid var(--ink);
  box-shadow:6px 6px 0 var(--ink);
  transition:transform .12s ease, box-shadow .12s ease, background .12s ease;
}
.card:hover{transform:translate(-2px,-2px); box-shadow:9px 9px 0 var(--ink); background:var(--stone-hi)}
.card:active{transform:translate(4px,4px); box-shadow:2px 2px 0 var(--ink)}
.card:focus-visible{outline:3px solid var(--face); outline-offset:3px}
.card.primary{background:var(--face); color:var(--ink)}
.card.primary:hover{background:#fff}
.card .glyph{
  font-size:1.7rem; line-height:1; width:2.6rem; height:2.6rem; flex:0 0 auto;
  display:grid; place-items:center; background:rgba(0,0,0,.16);
  border:2px solid var(--ink);
}
.card.primary .glyph{background:rgba(0,0,0,.08)}
.card .body{flex:1 1 auto; min-width:0; display:flex; flex-direction:column; gap:.28rem}
.card .act{font-weight:700; font-size:1.02rem; line-height:1.15}
.card .meta{
  font-family:"Silkscreen",ui-monospace,monospace; font-size:.66rem;
  color:var(--dim); letter-spacing:.03em;
}
.card.primary .meta{color:#4a4a52}
.card .arr{font-size:1.15rem; opacity:.55; flex:0 0 auto}
.hint-os{
  font-family:"Silkscreen",ui-monospace,monospace; font-size:.64rem;
  letter-spacing:.08em; color:var(--dim); text-align:center;
  margin:0 0 .85rem;
}
.note{
  margin-top:clamp(1.8rem,5vw,2.6rem); padding:1rem 1.15rem;
  background:var(--stone); border-left:3px solid var(--side);
  font-size:.9rem; color:var(--dim);
}
.note b{color:var(--face); font-weight:600}
.empty{
  text-align:center; padding:2.2rem 1rem; background:var(--stone);
  border:2px dashed var(--side); color:var(--dim);
}
footer{
  margin-top:2.4rem; text-align:center; font-size:.8rem; color:var(--dim);
}
footer a{color:var(--dim); text-decoration:underline; text-underline-offset:3px}
footer a:hover{color:var(--face)}
@keyframes rise{from{opacity:0; transform:translateY(10px)}to{opacity:1; transform:none}}
@media (prefers-reduced-motion:reduce){
  .logo{animation:none}
  .card{transition:none}
  .card:hover,.card:active{transform:none}
}
`

// renderDownloadPage собирает финальный HTML. Все динамические значения (версия,
// платформа) проходят через html.EscapeString; версия к тому же валидируется на
// уровне модели/сервиса, платформа — из allowlist.
func renderDownloadPage(cards []platformCard, osDetected bool) string {
	var b strings.Builder
	b.WriteString(`<!DOCTYPE html><html lang="ru"><head><meta charset="utf-8">`)
	b.WriteString(`<meta name="viewport" content="width=device-width, initial-scale=1">`)
	b.WriteString(`<title>Project Minecraft — скачать лаунчер</title>`)
	b.WriteString(`<meta name="robots" content="noindex">`)
	b.WriteString(`<link rel="preconnect" href="https://fonts.googleapis.com">`)
	b.WriteString(`<link rel="preconnect" href="https://fonts.gstatic.com" crossorigin>`)
	b.WriteString(`<link href="https://fonts.googleapis.com/css2?family=Inter:wght@400;600;700;800&family=Silkscreen:wght@400;700&display=swap" rel="stylesheet">`)
	b.WriteString(`<style>` + downloadPageCSS + `</style></head><body><main class="wrap">`)

	// Hero
	b.WriteString(`<div class="hero">`)
	b.WriteString(`<img class="logo" src="/download/pjm.png" width="416" height="416" alt="Project Minecraft">`)
	b.WriteString(`<div class="kicker">Project Minecraft</div>`)
	b.WriteString(`<h1>Лаунчер</h1>`)
	if len(cards) > 0 {
		b.WriteString(`<span class="ver">v` + html.EscapeString(cards[0].Version))
		b.WriteString(`</span>`)
	}
	b.WriteString(`</div>`)

	if len(cards) == 0 {
		b.WriteString(`<div class="empty">Сборки лаунчера сейчас готовятся —<br>загляните чуть позже.</div>`)
	} else {
		if osDetected {
			b.WriteString(`<p class="hint-os">Похоже, у вас ` + html.EscapeString(cards[0].Label) + ` — рекомендуем этот файл</p>`)
		}
		b.WriteString(`<div class="cards">`)
		for _, card := range cards {
			cls := "card"
			if card.Primary {
				cls += " primary"
			}
			b.WriteString(`<a class="` + cls + `" href="` + html.EscapeString(card.DownloadURL) + `" download>`)
			b.WriteString(`<span class="glyph" aria-hidden="true">` + card.Icon + `</span>`)
			b.WriteString(`<span class="body">`)
			b.WriteString(`<span class="act">Скачать для ` + html.EscapeString(card.Label) + `</span>`)
			meta := "v" + html.EscapeString(card.Version)
			if sz := humanSize(card.Size); sz != "" {
				meta += " · " + sz
			}
			b.WriteString(`<span class="meta">` + meta + `</span>`)
			b.WriteString(`</span>`)
			b.WriteString(`<span class="arr" aria-hidden="true">↓</span>`)
			b.WriteString(`</a>`)
		}
		b.WriteString(`</div>`)
	}

	b.WriteString(`<div class="note">Вход в лаунчер — по <b>учётной записи сайта</b>. ` +
		`Создать аккаунт и привязать его можно в Telegram-боте проекта.</div>`)
	b.WriteString(`<footer><a href="/privacy">Политика конфиденциальности</a></footer>`)
	b.WriteString(`</main></body></html>`)
	return b.String()
}
