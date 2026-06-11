package anticheat

import (
	"context"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gofiber/fiber/v3"
)

type fakeGate struct{ min string }

func (g fakeGate) MinMandatoryVersion(_ context.Context) (string, error) {
	return g.min, nil
}

func newGateApp(t *testing.T, min string) *fiber.App {
	t.Helper()
	app := fiber.New()
	passthrough := func(c fiber.Ctx) error { return c.Next() }
	NewHandler(nil).WithVersionGate(fakeGate{min: min}).RegisterRoutes(app, passthrough)
	return app
}

func postInit(t *testing.T, app *fiber.App, version string) int {
	t.Helper()
	req := httptest.NewRequest("POST", "/api/anticheat/handshake/init", strings.NewReader("{}"))
	req.Header.Set("Content-Type", "application/json")
	if version != "" {
		req.Header.Set("X-Launcher-Version", version)
	}
	res, err := app.Test(req)
	if err != nil {
		t.Fatalf("app.Test: %v", err)
	}
	return res.StatusCode
}

func TestVersionGateBlocksOutdatedLauncher(t *testing.T) {
	app := newGateApp(t, "0.2.0")

	if status := postInit(t, app, "0.1.0"); status != 426 {
		t.Fatalf("outdated client status = %d, want 426", status)
	}
	// Без заголовка = легаси-лаунчер 0.1.0 — тоже блокируется.
	if status := postInit(t, app, ""); status != 426 {
		t.Fatalf("legacy client status = %d, want 426", status)
	}
	// Актуальная версия проходит гейт (и падает дальше на отсутствии юзера = 401).
	if status := postInit(t, app, "0.2.0"); status != 401 {
		t.Fatalf("current client status = %d, want 401 (прошёл гейт)", status)
	}
}

func TestVersionGateInactiveWithoutMandatory(t *testing.T) {
	// Нет обязательных релизов — гейт пропускает даже без заголовка.
	app := newGateApp(t, "")
	if status := postInit(t, app, ""); status != 401 {
		t.Fatalf("status = %d, want 401 (гейт неактивен)", status)
	}
}
