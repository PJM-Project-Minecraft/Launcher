package gameapi_test

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"testing"
	"time"

	"launcher-backend/internal/gameapi"
	"launcher-backend/internal/models"
	"launcher-backend/internal/purchases"

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
	if err := db.AutoMigrate(&models.PlayerProfile{}, &models.PlayerXpAdjustment{}, &models.Order{}, &models.Delivery{}); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	db.Exec("DELETE FROM player_profiles")
	db.Exec("DELETE FROM player_xp_adjustments")
	db.Exec("DELETE FROM deliveries")
	db.Exec("DELETE FROM orders")

	app := fiber.New()
	gameapi.NewHandler(db, secret, purchases.NewService(db)).RegisterRoutes(app)
	return app, db
}

func TestDeliveryPollReturnsOnlyPendingForOnlinePlayers(t *testing.T) {
	app, db := newApp(t)
	orders := []models.Order{
		{OrderID: "11111111-1111-4111-8111-111111111111", Nick: "Liko", Items: []models.OrderItem{{ID: 4150, Name: "БПЛА", Qty: 1, Price: 55000}}, Total: 55000, YooKassaID: "delivery-pay-1", Status: models.OrderStatusPaid, PaidAt: time.Now()},
		{OrderID: "22222222-2222-4222-8222-222222222222", Nick: "Offline", Items: []models.OrderItem{{ID: 4159, Name: "РЭБ", Qty: 1, Price: 25000}}, Total: 25000, YooKassaID: "delivery-pay-2", Status: models.OrderStatusPaid, PaidAt: time.Now()},
	}
	if err := db.Create(&orders).Error; err != nil {
		t.Fatal(err)
	}
	deliveries := []models.Delivery{
		{OrderID: orders[0].OrderID, ItemIndex: 0, Nick: "Liko", Delivery: models.DeliverySpec{Type: models.DeliveryTypeRole, RoleID: "uav_operator"}, Status: models.DeliveryStatusPending},
		{OrderID: orders[0].OrderID, ItemIndex: 1, Nick: "Liko", Delivery: models.DeliverySpec{Type: models.DeliveryTypeItem, ItemID: "minecraft:diamond", Count: 1}, Status: models.DeliveryStatusDone},
		{OrderID: orders[1].OrderID, Nick: "Offline", Delivery: models.DeliverySpec{Type: models.DeliveryTypeRole, RoleID: "ew_specialist"}, Status: models.DeliveryStatusPending},
	}
	if err := db.Create(&deliveries).Error; err != nil {
		t.Fatal(err)
	}

	code, out := post(t, app, "/api/game/deliveries/poll", `{"players":["liko"]}`, secret)
	if code != http.StatusOK {
		t.Fatalf("poll: %d %s", code, out)
	}
	var response struct {
		Deliveries []models.Delivery `json:"deliveries"`
	}
	if err := json.Unmarshal([]byte(out), &response); err != nil {
		t.Fatal(err)
	}
	if len(response.Deliveries) != 1 || response.Deliveries[0].ID != deliveries[0].ID {
		t.Fatalf("ожидалась только pending-выдача онлайн-игрока: %+v", response.Deliveries)
	}
	code, out = post(t, app, "/api/game/deliveries/poll", `{"players":["Liko"]}`, secret)
	if code != http.StatusOK {
		t.Fatalf("second poll: %d %s", code, out)
	}
	if err := json.Unmarshal([]byte(out), &response); err != nil || len(response.Deliveries) != 0 {
		t.Fatalf("заклеймленная выдача повторилась: %+v err=%v", response.Deliveries, err)
	}
}

func TestDeliveryAckIsIdempotentAndIssuesOrderAfterAllDone(t *testing.T) {
	app, db := newApp(t)
	order := models.Order{OrderID: "33333333-3333-4333-8333-333333333333", Nick: "Liko", Items: []models.OrderItem{{ID: 4150, Name: "БПЛА", Qty: 1, Price: 55000}}, Total: 55000, YooKassaID: "delivery-pay-3", Status: models.OrderStatusPaid, PaidAt: time.Now()}
	if err := db.Create(&order).Error; err != nil {
		t.Fatal(err)
	}
	deliveries := []models.Delivery{
		{OrderID: order.OrderID, ItemIndex: 0, Nick: order.Nick, Delivery: models.DeliverySpec{Type: models.DeliveryTypeRole, RoleID: "uav_operator"}, Status: models.DeliveryStatusPending},
		{OrderID: order.OrderID, ItemIndex: 1, Nick: order.Nick, Delivery: models.DeliverySpec{Type: models.DeliveryTypeItem, ItemID: "minecraft:diamond", Count: 2}, Status: models.DeliveryStatusPending},
	}
	if err := db.Create(&deliveries).Error; err != nil {
		t.Fatal(err)
	}

	first := `{"ids":[` + strconv.FormatUint(uint64(deliveries[0].ID), 10) + `]}`
	if code, out := post(t, app, "/api/game/deliveries/ack", first, secret); code != http.StatusOK {
		t.Fatalf("first ack: %d %s", code, out)
	}
	db.First(&order, "order_id = ?", order.OrderID)
	if order.Status != models.OrderStatusPaid {
		t.Fatalf("заказ выдан до завершения всех выдач: %+v", order)
	}

	second := `{"ids":[` + strconv.FormatUint(uint64(deliveries[0].ID), 10) + `,` + strconv.FormatUint(uint64(deliveries[1].ID), 10) + `]}`
	for i := 0; i < 2; i++ {
		if code, out := post(t, app, "/api/game/deliveries/ack", second, secret); code != http.StatusOK {
			t.Fatalf("ack %d: %d %s", i, code, out)
		}
	}
	db.First(&order, "order_id = ?", order.OrderID)
	if order.Status != models.OrderStatusIssued || order.IssuedAt == nil {
		t.Fatalf("заказ не перешёл в issued: %+v", order)
	}
	var pending int64
	db.Model(&models.Delivery{}).Where("order_id = ? AND status = ?", order.OrderID, models.DeliveryStatusPending).Count(&pending)
	if pending != 0 {
		t.Fatalf("осталось pending-выдач: %d", pending)
	}
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

// TestSyncKeepsNameWhenIncomingEmpty — пустой ник не затирает уже известный.
// Мод шлёт "" при промахе кеша профилей у офлайн-игрока (а не «ника нет»), и
// массовый сброс рангов помечает грязными всех исторических игроков разом —
// безусловный upsert колонки name обнулил бы ники всей таблицы.
func TestSyncKeepsNameWhenIncomingEmpty(t *testing.T) {
	app, db := newApp(t)
	first := `{"players":[{"uuid":"33333333-3333-3333-3333-333333333333","name":"Liko","xp":500,"rankId":"sergeant"}]}`
	post(t, app, "/api/game/players/sync", first, secret)

	second := `{"players":[{"uuid":"33333333-3333-3333-3333-333333333333","name":"","xp":0,"rankId":"recruit"}]}`
	if code, out := post(t, app, "/api/game/players/sync", second, secret); code != http.StatusOK {
		t.Fatalf("ожидался 200, получен %d: %s", code, out)
	}

	var profile models.PlayerProfile
	if err := db.First(&profile, "uuid = ?", "33333333-3333-3333-3333-333333333333").Error; err != nil {
		t.Fatalf("find: %v", err)
	}
	if profile.Name != "Liko" {
		t.Fatalf("пустой ник затёр хороший: %+v", profile)
	}
	if profile.XP != 0 || profile.RankID != "recruit" {
		t.Fatalf("остальные колонки обязаны обновиться: %+v", profile)
	}
}

// TestSyncLargeBatchIsChunked — вайв на много профилей уходит нарезкой, а не одним
// statement: на Postgres неограниченный многострочный INSERT пробивает потолок в
// 65535 параметров (~13k профилей × 5 колонок), запрос падает, мод ретраит вечно.
func TestSyncLargeBatchIsChunked(t *testing.T) {
	app, db := newApp(t)
	var sb strings.Builder
	sb.WriteString(`{"players":[`)
	const count = 1200 // заведомо больше размера чанка (500)
	for i := 0; i < count; i++ {
		if i > 0 {
			sb.WriteByte(',')
		}
		sb.WriteString(`{"uuid":"` + fmt.Sprintf("00000000-0000-0000-0000-%012d", i) +
			`","name":"p` + strconv.Itoa(i) + `","xp":` + strconv.Itoa(i) + `,"rankId":"private"}`)
	}
	sb.WriteString(`]}`)

	if code, out := post(t, app, "/api/game/players/sync", sb.String(), secret); code != http.StatusOK {
		t.Fatalf("ожидался 200, получен %d: %s", code, out)
	}
	var total int64
	db.Model(&models.PlayerProfile{}).Count(&total)
	if total != count {
		t.Fatalf("сохранено %d из %d", total, count)
	}
}
