package launcherrelease

import (
	"bytes"
	"encoding/json"
	"io"
	"mime/multipart"
	"net/http/httptest"
	"testing"

	"launcher-backend/internal/events"
	"launcher-backend/internal/models"

	"github.com/gofiber/fiber/v3"
)

func newTestApp(t *testing.T) (*fiber.App, Service, *events.Broker) {
	t.Helper()
	service := newTestService(t)
	broker := events.NewBroker()
	app := fiber.New(fiber.Config{BodyLimit: 512 * 1024 * 1024})
	// passthrough инжектирует фиктивного admin-пользователя, чтобы auth.RequireAdmin пропустил запрос.
	passthrough := func(c fiber.Ctx) error {
		c.Locals("current-user", models.User{Login: "testadmin", Role: "admin"})
		return c.Next()
	}
	NewHandler(service, broker).RegisterRoutes(app, passthrough)
	return app, service, broker
}

func TestCreateAndCheckUpdateViaHTTP(t *testing.T) {
	app, _, broker := newTestApp(t)

	// Подписываемся на брокер: создание релиза должно публиковать событие.
	subID, ch := broker.Subscribe()
	defer broker.Unsubscribe(subID)

	body := &bytes.Buffer{}
	writer := multipart.NewWriter(body)
	_ = writer.WriteField("version", "0.2.0")
	_ = writer.WriteField("changelog", "первый авто-релиз")
	_ = writer.WriteField("mandatory", "true")
	part, _ := writer.CreateFormFile("linux-x64", "launcher")
	_, _ = part.Write([]byte("fake-binary"))
	_ = writer.Close()

	req := httptest.NewRequest("POST", "/api/admin/releases/", body)
	req.Header.Set("Content-Type", writer.FormDataContentType())
	res, err := app.Test(req)
	if err != nil {
		t.Fatalf("app.Test: %v", err)
	}
	if res.StatusCode != 201 {
		raw, _ := io.ReadAll(res.Body)
		t.Fatalf("create status = %d, body = %s", res.StatusCode, raw)
	}

	select {
	case msg := <-ch:
		if msg != "launcher-release" {
			t.Fatalf("broker event = %q, want launcher-release", msg)
		}
	default:
		t.Fatal("broker event not published on release create")
	}

	// Проверка обновления старым клиентом.
	req = httptest.NewRequest("GET", "/api/launcher/update?platform=linux-x64&version=0.1.0", nil)
	res, err = app.Test(req)
	if err != nil {
		t.Fatalf("app.Test: %v", err)
	}
	var info UpdateInfo
	if err := json.NewDecoder(res.Body).Decode(&info); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if !info.UpdateAvailable || info.LatestVersion != "0.2.0" || !info.Mandatory {
		t.Fatalf("info = %+v", info)
	}

	// Скачивание бинарника.
	req = httptest.NewRequest("GET", "/api/launcher/download/0.2.0/linux-x64", nil)
	res, err = app.Test(req)
	if err != nil {
		t.Fatalf("app.Test: %v", err)
	}
	if res.StatusCode != 200 {
		t.Fatalf("download status = %d", res.StatusCode)
	}
	raw, _ := io.ReadAll(res.Body)
	if string(raw) != "fake-binary" {
		t.Fatalf("downloaded = %q", raw)
	}
}

func TestCreateRejectsBadVersion(t *testing.T) {
	app, _, _ := newTestApp(t)

	body := &bytes.Buffer{}
	writer := multipart.NewWriter(body)
	_ = writer.WriteField("version", "не-версия")
	part, _ := writer.CreateFormFile("linux-x64", "launcher")
	_, _ = part.Write([]byte("x"))
	_ = writer.Close()

	req := httptest.NewRequest("POST", "/api/admin/releases/", body)
	req.Header.Set("Content-Type", writer.FormDataContentType())
	res, err := app.Test(req)
	if err != nil {
		t.Fatalf("app.Test: %v", err)
	}
	if res.StatusCode != 400 {
		t.Fatalf("status = %d, want 400", res.StatusCode)
	}
}
