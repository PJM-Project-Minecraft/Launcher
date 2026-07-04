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
