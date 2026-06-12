package anticheat

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"launcher-backend/internal/auth"
	"launcher-backend/internal/models"
	"launcher-backend/internal/yggdrasil"

	"github.com/gofiber/fiber/v3"
	"github.com/golang-jwt/jwt/v5"
	"github.com/google/uuid"
)

// TestEnforcementFlow проверяет ключевое утверждение M2: игрок проходит на сервер
// (hasJoined) только если игровая сессия прошла античит-confirm. Без confirm join
// отклоняется (403) — пропатченный лаунчер без агента не пустит игрока.
func TestEnforcementFlow(t *testing.T) {
	const jwtSecret = "test-jwt"
	const acSecret = "test-ac"

	db := newTestDB(t)
	if err := db.AutoMigrate(&models.User{}); err != nil {
		t.Fatalf("migrate user: %v", err)
	}
	user := models.User{ID: uuid.NewString(), Login: "Liko", ProviderUUID: "11111111-2222-3333-4444-555555555555", Role: "user"}
	if err := db.Create(&user).Error; err != nil {
		t.Fatalf("create user: %v", err)
	}

	keys, _ := yggdrasil.LoadOrCreateKey("/tmp/ac_int_key.pem")
	ygg := yggdrasil.NewService(db, keys, "http://example.com", "Test", "")

	app := fiber.New()
	authSvc := auth.NewService(db, auth.NewHTTPProvider(""), jwtSecret, nil, "test", 0)
	yggdrasil.NewHandler(ygg).RegisterRoutes(app, authSvc.RequireAuth())
	NewHandler(NewService(db, acSecret, false, ygg.Store(), "")).RegisterRoutes(app, authSvc.RequireAuth())

	jwtToken := mintJWT(t, jwtSecret, user.ID)

	// 1. handshake/init → launch-token + nonce.
	var initRes struct {
		Allowed     bool   `json:"allowed"`
		LaunchToken string `json:"launchToken"`
		Nonce       string `json:"nonce"`
	}
	doJSON(t, app, "POST", "/api/anticheat/handshake/init", jwtToken, `{"hwidHash":"hw-1"}`, http.StatusOK, &initRes)
	if !initRes.Allowed || initRes.LaunchToken == "" || initRes.Nonce == "" {
		t.Fatalf("init не выдал токен: %+v", initRes)
	}

	// 2. launcher-session с nonce → игровая сессия.
	var sess struct {
		AccessToken string `json:"accessToken"`
		UUID        string `json:"uuid"`
		Name        string `json:"name"`
	}
	doJSON(t, app, "POST", "/api/yggdrasil/launcher-session", jwtToken, `{"nonce":"`+initRes.Nonce+`"}`, http.StatusOK, &sess)
	if sess.AccessToken == "" {
		t.Fatal("сессия не выдана")
	}

	joinBody := `{"accessToken":"` + sess.AccessToken + `","selectedProfile":"` + sess.UUID + `","serverId":"srv-1"}`

	// 3. НЕГАТИВ: join до confirm → 403 (сессия не Verified).
	status, _ := do(t, app, "POST", "/api/yggdrasil/sessionserver/session/minecraft/join", "", joinBody)
	if status != http.StatusForbidden {
		t.Fatalf("join до confirm должен быть 403, получено %d", status)
	}

	// 4. confirm (стаб-агент).
	status, cbody := do(t, app, "POST", "/api/anticheat/handshake/confirm", "", `{"launchToken":"`+initRes.LaunchToken+`"}`)
	if status != http.StatusNoContent {
		t.Fatalf("confirm должен быть 204, получено %d (%s)", status, cbody)
	}

	// 5. join после confirm → 204.
	status, _ = do(t, app, "POST", "/api/yggdrasil/sessionserver/session/minecraft/join", "", joinBody)
	if status != http.StatusNoContent {
		t.Fatalf("join после confirm должен быть 204, получено %d", status)
	}

	// 6. hasJoined → 200 с профилем (сервер пускает игрока).
	status, body := do(t, app, "GET", "/api/yggdrasil/sessionserver/session/minecraft/hasJoined?username="+sess.Name+"&serverId=srv-1", "", "")
	if status != http.StatusOK || !strings.Contains(body, sess.UUID) {
		t.Fatalf("hasJoined должен пустить игрока: status=%d body=%s", status, body)
	}
}

// TestServerKickRevokesSession проверяет, что серверный kick (detect с системным
// типом inject) гасит игровую сессию по nonce — после P0-фикса nonce сохраняется при
// confirm, поэтому InvalidateByNonce находит сессию и последующий join отклоняется.
func TestServerKickRevokesSession(t *testing.T) {
	const jwtSecret = "test-jwt"
	const acSecret = "test-ac"

	db := newTestDB(t)
	if err := db.AutoMigrate(&models.User{}); err != nil {
		t.Fatalf("migrate user: %v", err)
	}
	user := models.User{ID: uuid.NewString(), Login: "LikoKick", ProviderUUID: "99999999-2222-3333-4444-555555555555", Role: "user"}
	if err := db.Create(&user).Error; err != nil {
		t.Fatalf("create user: %v", err)
	}

	keys, _ := yggdrasil.LoadOrCreateKey("/tmp/ac_kick_key.pem")
	ygg := yggdrasil.NewService(db, keys, "http://example.com", "Test", "")

	app := fiber.New()
	authSvc := auth.NewService(db, auth.NewHTTPProvider(""), jwtSecret, nil, "test", 0)
	yggdrasil.NewHandler(ygg).RegisterRoutes(app, authSvc.RequireAuth())
	NewHandler(NewService(db, acSecret, false, ygg.Store(), "")).RegisterRoutes(app, authSvc.RequireAuth())

	jwtToken := mintJWT(t, jwtSecret, user.ID)

	var initRes struct {
		Allowed     bool   `json:"allowed"`
		LaunchToken string `json:"launchToken"`
		Nonce       string `json:"nonce"`
	}
	doJSON(t, app, "POST", "/api/anticheat/handshake/init", jwtToken, `{"hwidHash":"hw-k"}`, http.StatusOK, &initRes)
	var sess struct {
		AccessToken string `json:"accessToken"`
		UUID        string `json:"uuid"`
		Name        string `json:"name"`
	}
	doJSON(t, app, "POST", "/api/yggdrasil/launcher-session", jwtToken, `{"nonce":"`+initRes.Nonce+`"}`, http.StatusOK, &sess)
	if status, body := do(t, app, "POST", "/api/anticheat/handshake/confirm", "", `{"launchToken":"`+initRes.LaunchToken+`"}`); status != http.StatusNoContent {
		t.Fatalf("confirm должен быть 204, получено %d (%s)", status, body)
	}

	joinBody := `{"accessToken":"` + sess.AccessToken + `","selectedProfile":"` + sess.UUID + `","serverId":"srv-k"}`
	if status, _ := do(t, app, "POST", "/api/yggdrasil/sessionserver/session/minecraft/join", "", joinBody); status != http.StatusNoContent {
		t.Fatalf("join после confirm должен быть 204, получено %d", status)
	}

	// Детект инъекции (системный тип → серверная severity 9 → kick).
	var detRes struct {
		Action string `json:"action"`
	}
	doJSON(t, app, "POST", "/api/anticheat/detect", "", `{"launchToken":"`+initRes.LaunchToken+`","source":"java","type":"inject","signature":"ghost","severity":1}`, http.StatusOK, &detRes)
	if detRes.Action != "kick" {
		t.Fatalf("inject-детект должен вернуть kick, получено %q", detRes.Action)
	}

	// После kick сессия погашена → повторный join отклоняется.
	if status, _ := do(t, app, "POST", "/api/yggdrasil/sessionserver/session/minecraft/join", "", joinBody); status == http.StatusNoContent {
		t.Fatal("после серверного kick join не должен проходить (сессия погашена)")
	}
}

func mintJWT(t *testing.T, secret, sub string) string {
	t.Helper()
	tok := jwt.NewWithClaims(jwt.SigningMethodHS256, jwt.MapClaims{
		"sub": sub,
		"exp": time.Now().Add(time.Hour).Unix(),
	})
	s, err := tok.SignedString([]byte(secret))
	if err != nil {
		t.Fatalf("mint jwt: %v", err)
	}
	return s
}

func do(t *testing.T, app *fiber.App, method, path, bearer, body string) (int, string) {
	t.Helper()
	var reader io.Reader
	if body != "" {
		reader = strings.NewReader(body)
	}
	req := httptest.NewRequest(method, path, reader)
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	if bearer != "" {
		req.Header.Set("Authorization", "Bearer "+bearer)
	}
	resp, err := app.Test(req, fiber.TestConfig{Timeout: 5 * time.Second})
	if err != nil {
		t.Fatalf("request %s %s: %v", method, path, err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(b)
}

func doJSON(t *testing.T, app *fiber.App, method, path, bearer, body string, wantStatus int, out any) {
	t.Helper()
	status, b := do(t, app, method, path, bearer, body)
	if status != wantStatus {
		t.Fatalf("%s %s: ожидался %d, получено %d (%s)", method, path, wantStatus, status, b)
	}
	if out != nil {
		if err := json.Unmarshal([]byte(b), out); err != nil {
			t.Fatalf("unmarshal %s: %v (%s)", path, err, b)
		}
	}
}
