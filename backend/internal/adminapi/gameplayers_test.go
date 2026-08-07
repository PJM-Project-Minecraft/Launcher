package adminapi_test

import (
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"

	"launcher-backend/internal/adminapi"
	"launcher-backend/internal/models"

	"github.com/gofiber/fiber/v3"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
)

func newAdminApp(t *testing.T) (*fiber.App, *gorm.DB) {
	t.Helper()
	db, err := gorm.Open(sqlite.Open("file:adminapi_game_test?mode=memory&cache=shared"), &gorm.Config{})
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	if err := db.AutoMigrate(&models.PlayerProfile{}, &models.PlayerXpAdjustment{}); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	db.Exec("DELETE FROM player_profiles")
	db.Exec("DELETE FROM player_xp_adjustments")

	app := fiber.New()
	// Пропускающий middleware: сам он ничего не проверяет (это предмет тестов auth), но
	// подкладывает фиктивного admin-пользователя в контекст — иначе auth.RequireAdmin,
	// навешенный внутри RegisterRoutes, отклонит запрос как неавторизованный (по образцу
	// launcherrelease/handler_test.go).
	passthrough := func(c fiber.Ctx) error {
		c.Locals("current-user", models.User{Login: "testadmin", Role: "admin"})
		return c.Next()
	}
	adminapi.NewHandler(db).RegisterRoutes(app, passthrough)
	return app, db
}

func TestListGamePlayers(t *testing.T) {
	app, db := newAdminApp(t)
	db.Create(&models.PlayerProfile{UUID: "11111111-1111-1111-1111-111111111111", Name: "Liko", XP: 500})
	db.Create(&models.PlayerProfile{UUID: "22222222-2222-2222-2222-222222222222", Name: "Petya", XP: 100})

	req, _ := http.NewRequest(http.MethodGet, "/api/admin/game/players?q=Lik", nil)
	resp, err := app.Test(req, fiber.TestConfig{Timeout: 0})
	if err != nil {
		t.Fatalf("test: %v", err)
	}
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("ожидался 200, получен %d: %s", resp.StatusCode, body)
	}
	var out struct {
		Items []models.PlayerProfile `json:"items"`
		Total int64                  `json:"total"`
	}
	if err := json.Unmarshal(body, &out); err != nil {
		t.Fatalf("json: %v (%s)", err, body)
	}
	if len(out.Items) != 1 || out.Items[0].Name != "Liko" {
		t.Fatalf("фильтр по нику не сработал: %+v", out.Items)
	}
}

func TestCreateAdjustment(t *testing.T) {
	app, db := newAdminApp(t)
	db.Create(&models.PlayerProfile{UUID: "11111111-1111-1111-1111-111111111111", Name: "Liko", XP: 500})

	req, _ := http.NewRequest(http.MethodPost,
		"/api/admin/game/players/11111111-1111-1111-1111-111111111111/adjust",
		strings.NewReader(`{"delta":250,"reason":"награда за ивент"}`))
	req.Header.Set("Content-Type", "application/json")
	resp, err := app.Test(req, fiber.TestConfig{Timeout: 0})
	if err != nil {
		t.Fatalf("test: %v", err)
	}
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("ожидался 201, получен %d: %s", resp.StatusCode, body)
	}

	var stored []models.PlayerXpAdjustment
	db.Find(&stored)
	if len(stored) != 1 || stored[0].Delta != 250 || stored[0].AppliedAt != nil {
		t.Fatalf("правка должна лежать неприменённой: %+v", stored)
	}
}

func TestCreateAdjustmentRejectsEmpty(t *testing.T) {
	app, _ := newAdminApp(t)
	req, _ := http.NewRequest(http.MethodPost,
		"/api/admin/game/players/11111111-1111-1111-1111-111111111111/adjust",
		strings.NewReader(`{"delta":0,"reason":"пусто"}`))
	req.Header.Set("Content-Type", "application/json")
	resp, _ := app.Test(req, fiber.TestConfig{Timeout: 0})
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("правка без delta и без setValue должна отклоняться, получен %d", resp.StatusCode)
	}
}

// TestCreateAdjustmentRejectsBadUUID — опечатка в UUID не должна давать 201.
// Мод на нераспознанный uuid отвечает единственным возможным способом — ACK без
// начисления, — поэтому строка получила бы applied_at, хотя XP никуда не двигался.
func TestCreateAdjustmentRejectsBadUUID(t *testing.T) {
	app, db := newAdminApp(t)
	req, _ := http.NewRequest(http.MethodPost,
		"/api/admin/game/players/не-uuid/adjust",
		strings.NewReader(`{"delta":250,"reason":"опечатка"}`))
	req.Header.Set("Content-Type", "application/json")
	resp, err := app.Test(req, fiber.TestConfig{Timeout: 0})
	if err != nil {
		t.Fatalf("test: %v", err)
	}
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("ожидался 400, получен %d", resp.StatusCode)
	}
	var count int64
	db.Model(&models.PlayerXpAdjustment{}).Count(&count)
	if count != 0 {
		t.Fatalf("битый uuid не должен порождать запись в очереди, создано %d", count)
	}
}
