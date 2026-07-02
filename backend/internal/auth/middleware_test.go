package auth

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"launcher-backend/internal/models"

	"github.com/gofiber/fiber/v3"
	"github.com/google/uuid"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
)

func newAuthTestService(t *testing.T) Service {
	t.Helper()
	dsn := "file:" + strings.ReplaceAll(t.Name(), "/", "_") + "?mode=memory&cache=shared"
	db, err := gorm.Open(sqlite.Open(dsn), &gorm.Config{})
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	if err := db.AutoMigrate(&models.User{}); err != nil {
		t.Fatalf("AutoMigrate: %v", err)
	}
	return NewService(db, nil, "test-secret", nil, "test", time.Hour)
}

func makeUser(t *testing.T, s Service, u models.User) models.User {
	t.Helper()
	u.ID = uuid.NewString()
	if u.Login == "" {
		u.Login = "login-" + u.ID[:8]
	}
	if u.ProviderUUID == "" {
		u.ProviderUUID = u.ID
	}
	if u.Role == "" {
		u.Role = models.RoleUser
	}
	if err := s.db.Create(&u).Error; err != nil {
		t.Fatalf("create user: %v", err)
	}
	return u
}

func requireAuthStatus(t *testing.T, s Service, token string) int {
	t.Helper()
	app := fiber.New()
	app.Get("/", s.RequireAuth(), func(c fiber.Ctx) error {
		return c.SendString("ok")
	})
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := app.Test(req)
	if err != nil {
		t.Fatalf("app.Test: %v", err)
	}
	return resp.StatusCode
}

func TestRequireAuthRejectsBanned(t *testing.T) {
	s := newAuthTestService(t)
	exp := time.Now().Add(time.Hour)

	ok := makeUser(t, s, models.User{})
	banned := makeUser(t, s, models.User{IsBanned: true})
	hwidBanned := makeUser(t, s, models.User{IsHwidBanned: true})

	tokOK, err := s.issueToken(ok, exp)
	if err != nil {
		t.Fatalf("issueToken: %v", err)
	}
	tokBan, err := s.issueToken(banned, exp)
	if err != nil {
		t.Fatalf("issueToken: %v", err)
	}
	tokHwid, err := s.issueToken(hwidBanned, exp)
	if err != nil {
		t.Fatalf("issueToken: %v", err)
	}

	if got := requireAuthStatus(t, s, tokOK); got != http.StatusOK {
		t.Errorf("незабаненный: got %d, want 200", got)
	}
	if got := requireAuthStatus(t, s, tokBan); got != http.StatusForbidden {
		t.Errorf("IsBanned: got %d, want 403", got)
	}
	if got := requireAuthStatus(t, s, tokHwid); got != http.StatusForbidden {
		t.Errorf("IsHwidBanned: got %d, want 403", got)
	}
	if got := requireAuthStatus(t, s, ""); got != http.StatusUnauthorized {
		t.Errorf("нет токена: got %d, want 401", got)
	}
}
