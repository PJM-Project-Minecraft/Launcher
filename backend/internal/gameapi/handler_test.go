package gameapi_test

import (
	"encoding/json"
	"io"
	"net/http"
	"strconv"
	"strings"
	"testing"

	"launcher-backend/internal/gameapi"
	"launcher-backend/internal/models"

	"github.com/gofiber/fiber/v3"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
)

const secret = "test-game-secret"

func newApp(t *testing.T) (*fiber.App, *gorm.DB) {
	t.Helper()
	db, err := gorm.Open(sqlite.Open("file:gameapi_test?mode=memory&cache=shared"), &gorm.Config{})
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	if err := db.AutoMigrate(&models.PlayerProfile{}, &models.PlayerXpAdjustment{}); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	db.Exec("DELETE FROM player_profiles")
	db.Exec("DELETE FROM player_xp_adjustments")

	app := fiber.New()
	gameapi.NewHandler(db, secret).RegisterRoutes(app)
	return app, db
}

func post(t *testing.T, app *fiber.App, path, body, hdr string) (int, string) {
	t.Helper()
	req, _ := http.NewRequest(http.MethodPost, path, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	if hdr != "" {
		req.Header.Set("X-Game-Secret", hdr)
	}
	resp, err := app.Test(req, fiber.TestConfig{Timeout: 0})
	if err != nil {
		t.Fatalf("test request: %v", err)
	}
	data, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(data)
}

func TestSyncRejectsWrongSecret(t *testing.T) {
	app, _ := newApp(t)
	code, _ := post(t, app, "/api/game/players/sync", `{"players":[]}`, "wrong")
	if code != http.StatusUnauthorized {
		t.Fatalf("ожидался 401, получен %d", code)
	}
}

func TestSyncUpsertsProfile(t *testing.T) {
	app, db := newApp(t)
	body := `{"players":[{"uuid":"11111111-1111-1111-1111-111111111111","name":"Liko","xp":500,"rankId":"sergeant"}]}`
	if code, out := post(t, app, "/api/game/players/sync", body, secret); code != http.StatusOK {
		t.Fatalf("ожидался 200, получен %d: %s", code, out)
	}
	// Повторный синк с другим XP — обновление, не дубль.
	body2 := `{"players":[{"uuid":"11111111-1111-1111-1111-111111111111","name":"Liko","xp":900,"rankId":"lieutenant"}]}`
	post(t, app, "/api/game/players/sync", body2, secret)

	var profiles []models.PlayerProfile
	db.Find(&profiles)
	if len(profiles) != 1 {
		t.Fatalf("ожидалась одна запись, получено %d", len(profiles))
	}
	if profiles[0].XP != 900 || profiles[0].RankID != "lieutenant" {
		t.Fatalf("upsert не обновил запись: %+v", profiles[0])
	}
}

// TestSyncDedupesDuplicateUuidWithinBatch — один батч с двумя записями одного uuid.
// На Postgres многострочный INSERT ... ON CONFLICT DO UPDATE падает с
// "cannot affect row a second time", если задеть один conflict-key дважды в одном
// statement — вернулся бы 500 и потерялись бы ВСЕ игроки батча, не только дубль.
// SQLite (тестовая БД) этого ограничения не соблюдает, поэтому RED здесь получить
// нельзя честно — тест проверяет результат по содержимому таблицы (последняя запись
// в батче побеждает) как регрессионную защиту дедупликации, а не по коду ответа.
func TestSyncDedupesDuplicateUuidWithinBatch(t *testing.T) {
	app, db := newApp(t)
	body := `{"players":[` +
		`{"uuid":"22222222-2222-2222-2222-222222222222","name":"Liko","xp":100,"rankId":"private"},` +
		`{"uuid":"22222222-2222-2222-2222-222222222222","name":"Liko","xp":700,"rankId":"captain"}` +
		`]}`
	if code, out := post(t, app, "/api/game/players/sync", body, secret); code != http.StatusOK {
		t.Fatalf("ожидался 200, получен %d: %s", code, out)
	}

	var profiles []models.PlayerProfile
	db.Find(&profiles)
	if len(profiles) != 1 {
		t.Fatalf("ожидалась одна запись после дедупликации, получено %d: %+v", len(profiles), profiles)
	}
	if profiles[0].XP != 700 || profiles[0].RankID != "captain" {
		t.Fatalf("должна победить последняя запись батча: %+v", profiles[0])
	}
}

func TestPollReturnsPendingAndAcksApplied(t *testing.T) {
	app, db := newApp(t)
	adj := models.PlayerXpAdjustment{UUID: "11111111-1111-1111-1111-111111111111", Delta: 250, Reason: "награда", CreatedBy: "admin"}
	if err := db.Create(&adj).Error; err != nil {
		t.Fatalf("create: %v", err)
	}

	code, out := post(t, app, "/api/game/adjustments/poll", `{"ack":[]}`, secret)
	if code != http.StatusOK {
		t.Fatalf("ожидался 200, получен %d: %s", code, out)
	}
	var first struct {
		Adjustments []models.PlayerXpAdjustment `json:"adjustments"`
	}
	if err := json.Unmarshal([]byte(out), &first); err != nil {
		t.Fatalf("json: %v (%s)", err, out)
	}
	if len(first.Adjustments) != 1 || first.Adjustments[0].Delta != 250 {
		t.Fatalf("ожидалась одна правка на +250, получено %+v", first.Adjustments)
	}

	// ACK по id — правка больше не возвращается.
	_, out2 := post(t, app, "/api/game/adjustments/poll",
		`{"ack":[`+strconv.FormatUint(uint64(first.Adjustments[0].ID), 10)+`]}`, secret)
	var second struct {
		Adjustments []models.PlayerXpAdjustment `json:"adjustments"`
	}
	if err := json.Unmarshal([]byte(out2), &second); err != nil {
		t.Fatalf("json: %v (%s)", err, out2)
	}
	if len(second.Adjustments) != 0 {
		t.Fatalf("после ACK правок быть не должно, получено %+v", second.Adjustments)
	}
}
