package yggdrasil

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"launcher-backend/internal/models"

	"github.com/gofiber/fiber/v3"
)

// keepalive продлевает живую сессию по nonce (лаунчер держит её живой, пока игра
// запущена) и не воскрешает неизвестную/истёкшую (404).
func TestKeepaliveEndpoint(t *testing.T) {
	svc := newTestService()
	svc.IssueSession(models.User{Login: "Liko", ProviderUUID: "u"}, "nonce-ka")

	app := fiber.New()
	noAuth := func(c fiber.Ctx) error { return c.Next() }
	NewHandler(svc).RegisterRoutes(app, noAuth)

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

	if code := post(`{"nonce":"nonce-ka"}`); code != http.StatusNoContent {
		t.Fatalf("живой nonce: ожидался 204, получен %d", code)
	}
	if code := post(`{"nonce":"never-issued"}`); code != http.StatusNotFound {
		t.Fatalf("неизвестный nonce: ожидался 404, получен %d", code)
	}
}
