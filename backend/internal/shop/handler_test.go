package shop_test

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"io"
	"mime/multipart"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"launcher-backend/internal/models"
	"launcher-backend/internal/shop"

	"github.com/gofiber/fiber/v3"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
)

func newApp(t *testing.T) (*fiber.App, *gorm.DB, string) {
	t.Helper()
	db, err := gorm.Open(sqlite.Open("file:"+t.Name()+"?mode=memory&cache=shared"), &gorm.Config{})
	if err != nil {
		t.Fatal(err)
	}
	if err := db.AutoMigrate(&models.ShopItem{}); err != nil {
		t.Fatal(err)
	}
	root := t.TempDir()
	service := shop.NewService(db, root)
	app := fiber.New()
	admin := func(c fiber.Ctx) error {
		c.Locals("current-user", models.User{Role: "admin"})
		return c.Next()
	}
	shop.NewHandler(service).RegisterRoutes(app, admin)
	return app, db, root
}

func request(t *testing.T, app *fiber.App, method, path, body, contentType string) (int, []byte) {
	t.Helper()
	req, _ := http.NewRequest(method, path, strings.NewReader(body))
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	resp, err := app.Test(req, fiber.TestConfig{Timeout: 0})
	if err != nil {
		t.Fatal(err)
	}
	data, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, data
}

func TestPublicCatalogOnlyExposesActiveItems(t *testing.T) {
	app, db, _ := newApp(t)
	rows := []models.ShopItem{
		{ID: 4150, Category: "Привилегии", CategoryIcon: "star", Name: "БПЛА", Description: "x", Price: 55000, Sort: 1, Active: true, Delivery: models.DeliverySpec{Type: models.DeliveryTypeRole, RoleID: "uav_operator"}},
		{ID: 4159, Category: "Привилегии", CategoryIcon: "star", Name: "РЭБ", Description: "x", Price: 25000, Sort: 2, Active: false, Delivery: models.DeliverySpec{Type: models.DeliveryTypeRole, RoleID: "ew_specialist"}},
	}
	if err := db.Create(&rows).Error; err != nil {
		t.Fatal(err)
	}

	code, data := request(t, app, http.MethodGet, "/api/shop/catalog", "", "")
	if code != http.StatusOK {
		t.Fatalf("catalog: %d %s", code, data)
	}
	var items []shop.CatalogItem
	if err := json.Unmarshal(data, &items); err != nil {
		t.Fatal(err)
	}
	if len(items) != 1 {
		t.Fatalf("публично отдано %d товаров", len(items))
	}
	for _, item := range items {
		if item.ID == 4159 {
			t.Fatal("неактивный товар попал в каталог")
		}
		if item.ID == 4150 && (item.Price != 55000 || item.Delivery.RoleID != "uav_operator") {
			t.Fatalf("денежный/Delivery seed неверен: %+v", item)
		}
	}
}

func TestAdminUpdatesWholeItemAndValidatesDelivery(t *testing.T) {
	app, db, _ := newApp(t)
	if err := db.Create(&models.ShopItem{ID: 4150, Category: "Привилегии", CategoryIcon: "star", Name: "БПЛА", Description: "x", Price: 55000, Active: true, Delivery: models.DeliverySpec{Type: models.DeliveryTypeRole, RoleID: "uav_operator"}}).Error; err != nil {
		t.Fatal(err)
	}
	bad := `{"category":"Предметы","categoryIcon":"blocks","name":"Алмаз","description":"x","price":10000,"sort":1,"active":true,"delivery":{"type":"item","itemId":"bad id","count":1}}`
	if code, _ := request(t, app, http.MethodPut, "/api/admin/shop/items/4150", bad, "application/json"); code != http.StatusBadRequest {
		t.Fatalf("битая выдача принята: %d", code)
	}
	good := `{"category":"Предметы","categoryIcon":"blocks","name":"Алмаз","description":"Описание","price":10000,"badge":"Хит","sort":3,"active":true,"delivery":{"type":"item","itemId":"minecraft:diamond","count":2}}`
	code, data := request(t, app, http.MethodPut, "/api/admin/shop/items/4150", good, "application/json")
	if code != http.StatusOK {
		t.Fatalf("update: %d %s", code, data)
	}
	var item shop.CatalogItem
	if err := json.Unmarshal(data, &item); err != nil {
		t.Fatal(err)
	}
	if item.Name != "Алмаз" || item.Price != 10000 || item.Delivery.Count != 2 {
		t.Fatalf("не обновлено: %+v", item)
	}
}

func TestAdminCreatesAndDeletesItem(t *testing.T) {
	app, db, _ := newApp(t)
	input := `{"id":90001,"category":"Предметы","categoryIcon":"blocks","name":"Алмаз","description":"x","price":10000,"sort":999,"active":true,"delivery":{"type":"item","itemId":"minecraft:diamond","count":1}}`
	code, data := request(t, app, http.MethodPost, "/api/admin/shop/items", input, "application/json")
	if code != http.StatusCreated {
		t.Fatalf("create: %d %s", code, data)
	}
	if code, data = request(t, app, http.MethodDelete, "/api/admin/shop/items/90001", "", ""); code != http.StatusNoContent {
		t.Fatalf("delete: %d %s", code, data)
	}
	var count int64
	if err := db.Model(&models.ShopItem{}).Where("id = ?", 90001).Count(&count).Error; err != nil || count != 0 {
		t.Fatalf("товар не удалён: count=%d err=%v", count, err)
	}
}

func TestAdminUploadsPngAndPublicRouteServesIt(t *testing.T) {
	app, db, root := newApp(t)
	if err := db.Create(&models.ShopItem{ID: 4150, Category: "Привилегии", CategoryIcon: "star", Name: "БПЛА", Description: "x", Price: 55000, Active: true, Delivery: models.DeliverySpec{Type: models.DeliveryTypeNone}}).Error; err != nil {
		t.Fatal(err)
	}
	png, _ := base64.StdEncoding.DecodeString("iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAQAAAC1HAwCAAAAC0lEQVR42mNk+A8AAQUBAScY42YAAAAASUVORK5CYII=")
	var body bytes.Buffer
	writer := multipart.NewWriter(&body)
	part, _ := writer.CreateFormFile("image", "item.png")
	_, _ = part.Write(png)
	_ = writer.Close()
	req, _ := http.NewRequest(http.MethodPost, "/api/admin/shop/items/4150/image", &body)
	req.Header.Set("Content-Type", writer.FormDataContentType())
	resp, err := app.Test(req, fiber.TestConfig{Timeout: 0})
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusOK {
		data, _ := io.ReadAll(resp.Body)
		t.Fatalf("upload: %d %s", resp.StatusCode, data)
	}
	if _, err := os.Stat(filepath.Join(root, "4150.png")); err != nil {
		t.Fatal(err)
	}
	var uploaded shop.CatalogItem
	data, _ := io.ReadAll(resp.Body)
	if err := json.Unmarshal(data, &uploaded); err != nil || !strings.Contains(uploaded.ImageURL, "?v=") {
		t.Fatalf("нет cache-busting URL: %s err=%v", data, err)
	}

	code, served := request(t, app, http.MethodGet, "/api/shop/images/4150.png", "", "")
	if code != http.StatusOK || !bytes.Equal(served, png) {
		t.Fatalf("image response: %d bytes=%d", code, len(served))
	}
}
