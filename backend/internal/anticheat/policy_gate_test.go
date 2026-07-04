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
