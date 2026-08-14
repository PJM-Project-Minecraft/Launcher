package purchases_test

import (
	"encoding/json"
	"io"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"

	"launcher-backend/internal/models"
	"launcher-backend/internal/purchases"

	"github.com/gofiber/fiber/v3"
	"gorm.io/driver/postgres"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
)

const siteSecret = "test-site-secret"

const (
	orderOne = "11111111-1111-4111-8111-111111111111"
	orderTwo = "22222222-2222-4222-8222-222222222222"
	orderOld = "33333333-3333-4333-8333-333333333333"
)

func newApp(t *testing.T) (*fiber.App, *gorm.DB) {
	t.Helper()
	db, err := gorm.Open(sqlite.Open("file:"+t.Name()+"?mode=memory&cache=shared"), &gorm.Config{})
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	if err := db.AutoMigrate(&models.Order{}, &models.BotAuditLog{}); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	app := fiber.New()
	admin := func(c fiber.Ctx) error {
		c.Locals("current-user", models.User{ID: orderOld, Role: "admin", Login: "tester"})
		return c.Next()
	}
	purchases.NewHandler(db, siteSecret).RegisterRoutes(app, admin)
	return app, db
}

func TestStatsPostgresSQL(t *testing.T) {
	dsn := os.Getenv("TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("TEST_DATABASE_URL not set")
	}
	db, err := gorm.Open(postgres.Open(dsn), &gorm.Config{})
	if err != nil {
		t.Fatalf("open postgres: %v", err)
	}
	if err := db.AutoMigrate(&models.Order{}); err != nil {
		t.Fatalf("migrate postgres: %v", err)
	}
	if err := db.Exec("DELETE FROM orders").Error; err != nil {
		t.Fatal(err)
	}
	orders := []models.Order{
		{OrderID: orderOne, Nick: "Liko", Items: []models.OrderItem{{ID: 1, Name: "A", Qty: 2, Price: 10000}}, Total: 20000, YooKassaID: "pg-1", Status: models.OrderStatusPaid, PaidAt: time.Date(2026, 8, 10, 0, 0, 0, 0, time.UTC)},
		{OrderID: orderTwo, Nick: "Other", Items: []models.OrderItem{{ID: 1, Name: "A", Qty: 1, Price: 10000}}, Total: 10000, YooKassaID: "pg-2", Status: models.OrderStatusIssued, PaidAt: time.Date(2026, 8, 11, 0, 0, 0, 0, time.UTC)},
	}
	if err := db.Create(&orders).Error; err != nil {
		t.Fatal(err)
	}
	stats, err := purchases.NewService(db).Stats(t.Context(), "2026-08-01", "2026-08-31")
	if err != nil {
		t.Fatalf("postgres stats: %v", err)
	}
	if stats.Revenue != 30000 || stats.Orders != 2 || len(stats.Top) != 1 || stats.Top[0].Qty != 3 {
		t.Fatalf("unexpected postgres stats: %+v", stats)
	}
}

func request(t *testing.T, app *fiber.App, method, path, body, secret string) (int, string) {
	t.Helper()
	req, _ := http.NewRequest(method, path, strings.NewReader(body))
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	if secret != "" {
		req.Header.Set("X-Site-Secret", secret)
	}
	resp, err := app.Test(req, fiber.TestConfig{Timeout: 0})
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	data, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(data)
}

func orderBody(id, nick string, total int64) string {
	return `{"orderId":"` + id + `","nick":"` + nick + `","items":[{"id":3965,"name":"Элитное снаряжение","qty":1,"price":` +
		jsonNumber(total) + `}],"total":` + jsonNumber(total) + `,"yookassaId":"pay-1","paidAt":"2026-08-14T12:00:00Z"}`
}

func jsonNumber(n int64) string {
	data, _ := json.Marshal(n)
	return string(data)
}

func TestCreateOrderRequiresSecretAndIsIdempotent(t *testing.T) {
	app, db := newApp(t)
	body := orderBody(orderOne, "Liko", 85000)

	if code, _ := request(t, app, http.MethodPost, "/api/site/orders", body, "wrong"); code != http.StatusUnauthorized {
		t.Fatalf("wrong secret: got %d", code)
	}
	if code, out := request(t, app, http.MethodPost, "/api/site/orders", body, siteSecret); code != http.StatusCreated {
		t.Fatalf("create: got %d: %s", code, out)
	}
	if code, out := request(t, app, http.MethodPost, "/api/site/orders", body, siteSecret); code != http.StatusOK {
		t.Fatalf("idempotent repeat: got %d: %s", code, out)
	}
	var count int64
	db.Model(&models.Order{}).Count(&count)
	if count != 1 {
		t.Fatalf("expected one order, got %d", count)
	}
}

func TestEmptyConfiguredSecretKeepsSiteEndpointClosed(t *testing.T) {
	db, err := gorm.Open(sqlite.Open("file:"+t.Name()+"?mode=memory&cache=shared"), &gorm.Config{})
	if err != nil {
		t.Fatal(err)
	}
	if err := db.AutoMigrate(&models.Order{}); err != nil {
		t.Fatal(err)
	}
	app := fiber.New()
	purchases.NewHandler(db, "").RegisterRoutes(app, func(c fiber.Ctx) error { return c.Next() })
	code, _ := request(t, app, http.MethodPost, "/api/site/orders", orderBody(orderOne, "Liko", 85000), "")
	if code != http.StatusUnauthorized {
		t.Fatalf("empty configured secret must reject requests, got %d", code)
	}
}

func TestCreateOrderValidatesServerPayload(t *testing.T) {
	app, _ := newApp(t)
	cases := []string{
		`{"orderId":"","nick":"Liko","items":[],"total":0,"yookassaId":"pay"}`,
		orderBody(orderOne, "Лико", 85000),
		`{"orderId":"` + orderOne + `","nick":"Liko","items":[{"id":1,"name":"x","qty":2,"price":100}],"total":100,"yookassaId":"pay"}`,
	}
	for _, body := range cases {
		if code, out := request(t, app, http.MethodPost, "/api/site/orders", body, siteSecret); code != http.StatusBadRequest {
			t.Fatalf("invalid payload accepted (%d): %s", code, out)
		}
	}
}

func TestListFiltersAndIssueAreIdempotent(t *testing.T) {
	app, db := newApp(t)
	for _, order := range []models.Order{
		{OrderID: orderOne, Nick: "Liko", Items: []models.OrderItem{{ID: 1, Name: "A", Qty: 1, Price: 10000}}, Total: 10000, YooKassaID: "pay-a", Status: models.OrderStatusPaid, PaidAt: time.Date(2026, 8, 10, 12, 0, 0, 0, time.UTC)},
		{OrderID: orderTwo, Nick: "Other", Items: []models.OrderItem{{ID: 2, Name: "B", Qty: 2, Price: 20000}}, Total: 40000, YooKassaID: "pay-b", Status: models.OrderStatusIssued, PaidAt: time.Date(2026, 8, 11, 12, 0, 0, 0, time.UTC)},
	} {
		if err := db.Create(&order).Error; err != nil {
			t.Fatal(err)
		}
	}

	code, out := request(t, app, http.MethodGet, "/api/admin/orders?status=paid&q=lik", "", "")
	if code != http.StatusOK {
		t.Fatalf("list: %d %s", code, out)
	}
	var list struct {
		Items []models.Order `json:"items"`
		Total int64          `json:"total"`
	}
	if err := json.Unmarshal([]byte(out), &list); err != nil {
		t.Fatal(err)
	}
	if list.Total != 1 || len(list.Items) != 1 || list.Items[0].OrderID != orderOne {
		t.Fatalf("unexpected filtered list: %+v", list)
	}
	code, out = request(t, app, http.MethodGet, "/api/admin/orders?from=2026-08-11&to=2026-08-11", "", "")
	if code != http.StatusOK {
		t.Fatalf("date-filtered list: %d %s", code, out)
	}
	if err := json.Unmarshal([]byte(out), &list); err != nil {
		t.Fatal(err)
	}
	if list.Total != 1 || len(list.Items) != 1 || list.Items[0].OrderID != orderTwo {
		t.Fatalf("unexpected date-filtered list: %+v", list)
	}

	for i := 0; i < 2; i++ {
		code, out = request(t, app, http.MethodPost, "/api/admin/orders/"+orderOne+"/issue", `{}`, "")
		if code != http.StatusOK {
			t.Fatalf("issue %d: %d %s", i, code, out)
		}
	}
	var issued models.Order
	db.First(&issued, "order_id = ?", orderOne)
	if issued.Status != models.OrderStatusIssued || issued.IssuedAt == nil {
		t.Fatalf("order not issued: %+v", issued)
	}
	var audits int64
	db.Model(&models.BotAuditLog{}).Where("action = ?", "admin_order_issue").Count(&audits)
	if audits != 1 {
		t.Fatalf("expected one audit record for idempotent issue, got %d", audits)
	}
}

func TestStatsUsesPeriodAndAggregatesTopItems(t *testing.T) {
	app, db := newApp(t)
	orders := []models.Order{
		{OrderID: orderOne, Nick: "Liko", Items: []models.OrderItem{{ID: 1, Name: "A", Qty: 2, Price: 10000}}, Total: 20000, YooKassaID: "p1", Status: models.OrderStatusPaid, PaidAt: time.Date(2026, 8, 10, 0, 0, 0, 0, time.UTC)},
		{OrderID: orderTwo, Nick: "Liko", Items: []models.OrderItem{{ID: 1, Name: "A", Qty: 1, Price: 10000}, {ID: 2, Name: "B", Qty: 1, Price: 5000}}, Total: 15000, YooKassaID: "p2", Status: models.OrderStatusIssued, PaidAt: time.Date(2026, 8, 11, 0, 0, 0, 0, time.UTC)},
		{OrderID: orderOld, Nick: "Old", Items: []models.OrderItem{{ID: 3, Name: "Old", Qty: 1, Price: 99900}}, Total: 99900, YooKassaID: "p3", Status: models.OrderStatusPaid, PaidAt: time.Date(2026, 7, 1, 0, 0, 0, 0, time.UTC)},
	}
	for i := range orders {
		if err := db.Create(&orders[i]).Error; err != nil {
			t.Fatal(err)
		}
	}
	code, out := request(t, app, http.MethodGet, "/api/admin/orders/stats?from=2026-08-01&to=2026-08-31", "", "")
	if code != http.StatusOK {
		t.Fatalf("stats: %d %s", code, out)
	}
	var stats struct {
		Revenue int64 `json:"revenue"`
		Orders  int64 `json:"orders"`
		Top     []struct {
			ID  int64 `json:"id"`
			Qty int   `json:"qty"`
		} `json:"topItems"`
	}
	if err := json.Unmarshal([]byte(out), &stats); err != nil {
		t.Fatal(err)
	}
	if stats.Revenue != 35000 || stats.Orders != 2 || len(stats.Top) == 0 || stats.Top[0].ID != 1 || stats.Top[0].Qty != 3 {
		t.Fatalf("unexpected stats: %+v", stats)
	}
}
