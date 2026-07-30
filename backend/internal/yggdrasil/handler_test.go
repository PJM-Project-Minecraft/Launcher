package yggdrasil

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"launcher-backend/internal/models"

	"github.com/gofiber/fiber/v3"
)

// keepalive продлевает живую сессию по nonce (лаунчер держит её живой, пока игра
// запущена) и не воскрешает неизвестную/истёкшую (404).
func TestKeepaliveEndpoint(t *testing.T) {
	svc := newTestService()
	svc.IssueSession(models.User{Login: "Liko", ProviderUUID: "u"}, "nonce-ka")

	app := fiber.New()
	// keepalive требует владельца сессии: nonce приходит из тела, и без привязки к
	// авторизованному игроку чужую сессию мог продлевать любой держатель JWT.
	asLiko := func(c fiber.Ctx) error {
		c.Locals("current-user", models.User{Login: "Liko", ProviderUUID: "u"})
		return c.Next()
	}
	NewHandler(svc).RegisterRoutes(app, asLiko)

	post := func(body string) int {
		req := httptest.NewRequest(http.MethodPost,
			"/api/yggdrasil/launcher-session/keepalive", strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		resp, err := app.Test(req)
		if err != nil {
			t.Fatalf("app.Test: %v", err)
		}
		return resp.StatusCode
	}

	before := time.Now()
	if svc.Store().LauncherSustainedAfter("nonce-ka", before) {
		t.Fatal("до keepalive отметки живости лаунчера быть не должно")
	}
	if code := post(`{"nonce":"nonce-ka"}`); code != http.StatusNoContent {
		t.Fatalf("живой nonce: ожидался 204, получен %d", code)
	}
	// Одна метка — ещё не устойчивая связь (может быть первый пинг после обрыва).
	if svc.Store().LauncherSustainedAfter("nonce-ka", before) {
		t.Fatal("одиночный keepalive не должен считаться устойчивой связью")
	}
	// Второй keepalive: связь держится — сигнал «игра идёт» для reaper.
	if code := post(`{"nonce":"nonce-ka"}`); code != http.StatusNoContent {
		t.Fatalf("живой nonce: ожидался 204, получен %d", code)
	}
	if !svc.Store().LauncherSustainedAfter("nonce-ka", before) {
		t.Fatal("после двух keepalive должна появиться отметка устойчивой связи")
	}
	if code := post(`{"nonce":"never-issued"}`); code != http.StatusNotFound {
		t.Fatalf("неизвестный nonce: ожидался 404, получен %d", code)
	}

	// Чужой nonce: сессия жива, но принадлежит другому игроку — продлевать нельзя.
	svc.IssueSession(models.User{Login: "Victim", ProviderUUID: "v"}, "nonce-victim")
	if code := post(`{"nonce":"nonce-victim"}`); code != http.StatusNotFound {
		t.Fatalf("чужой nonce: ожидался 404, получен %d", code)
	}
	if svc.Store().LauncherSustainedAfter("nonce-victim", before) {
		t.Fatal("чужой keepalive не должен отмечать живость лаунчера жертвы")
	}
}
