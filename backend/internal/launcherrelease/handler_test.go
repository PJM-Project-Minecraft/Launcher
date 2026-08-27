package launcherrelease

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"mime/multipart"
	"net"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"launcher-backend/internal/events"
	"launcher-backend/internal/models"

	"github.com/gofiber/fiber/v3"
)

type queuedPublisher struct{}

func (queuedPublisher) QueueLauncherRelease(_ context.Context, release models.LauncherRelease) (models.DeliveryJob, error) {
	return models.DeliveryJob{ID: "job-id", Kind: "launcher", Generation: release.ID, Status: "queued", Phase: "queued"}, nil
}

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

func TestReleaseUploadStreamsBeyondDefaultJSONBudget(t *testing.T) {
	service := newTestService(t)
	app := fiber.New(fiber.Config{
		BodyLimit:                    1024,
		StreamRequestBody:            true,
		DisablePreParseMultipartForm: true,
	})
	auth := func(c fiber.Ctx) error {
		c.Locals("current-user", models.User{Login: "testadmin", Role: "admin"})
		return c.Next()
	}
	NewHandler(service, nil).RegisterRoutes(app, auth)

	body := &bytes.Buffer{}
	writer := multipart.NewWriter(body)
	_ = writer.WriteField("version", "0.2.1")
	part, _ := writer.CreateFormFile("linux-x64", "launcher")
	_, _ = part.Write(bytes.Repeat([]byte("x"), 2048))
	_ = writer.Close()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer app.Shutdown()
	go func() { _ = app.Listener(listener, fiber.ListenConfig{DisableStartupMessage: true}) }()
	req, err := http.NewRequest(http.MethodPost, "http://"+listener.Addr().String()+"/api/admin/releases/", body)
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	req.Header.Set("Content-Type", writer.FormDataContentType())
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("http.Do: %v", err)
	}
	if res.StatusCode != 201 {
		raw, _ := io.ReadAll(res.Body)
		t.Fatalf("status=%d body=%s", res.StatusCode, raw)
	}
}

func TestV2UploadReturnsDurableJobBeforeActivation(t *testing.T) {
	service := newTestService(t)
	broker := events.NewBroker()
	subscriptionID, eventChannel := broker.Subscribe()
	defer broker.Unsubscribe(subscriptionID)
	app := fiber.New(fiber.Config{BodyLimit: 512 * 1024 * 1024})
	auth := func(c fiber.Ctx) error {
		c.Locals("current-user", models.User{Login: "testadmin", Role: "admin"})
		return c.Next()
	}
	NewHandler(service, broker, queuedPublisher{}).RegisterRoutesWithV1Bridge(app, auth, false)
	body := &bytes.Buffer{}
	writer := multipart.NewWriter(body)
	_ = writer.WriteField("version", "1.0.0")
	part, _ := writer.CreateFormFile("linux-x64", "launcher")
	_, _ = part.Write([]byte("binary"))
	_ = writer.Close()
	req := httptest.NewRequest("POST", "/api/v2/admin/launcher-releases/", body)
	req.Header.Set("Content-Type", writer.FormDataContentType())
	res, err := app.Test(req)
	if err != nil {
		t.Fatal(err)
	}
	if res.StatusCode != 202 {
		raw, _ := io.ReadAll(res.Body)
		t.Fatalf("status=%d body=%s", res.StatusCode, raw)
	}
	var job models.DeliveryJob
	if err := json.NewDecoder(res.Body).Decode(&job); err != nil || job.Status != "queued" {
		t.Fatalf("job=%+v err=%v", job, err)
	}
	var release models.LauncherRelease
	if err := service.db.Where("version = ?", "1.0.0").First(&release).Error; err != nil {
		t.Fatal(err)
	}
	if release.IsActive || release.PublishedAt != nil {
		t.Fatalf("staged upload entered channel before worker: %+v", release)
	}
	select {
	case event := <-eventChannel:
		t.Fatalf("staged upload published client event before activation: %q", event)
	default:
	}
}

func TestV1BridgeCanBeDisabledWithoutRemovingV2Admin(t *testing.T) {
	service := newTestService(t)
	app := fiber.New()
	auth := func(c fiber.Ctx) error {
		c.Locals("current-user", models.User{Login: "testadmin", Role: "admin"})
		return c.Next()
	}
	NewHandler(service, nil).RegisterRoutesWithV1Bridge(app, auth, false)

	legacy, err := app.Test(httptest.NewRequest("GET", "/api/launcher/update?platform=linux-x64&version=0.1.0", nil))
	if err != nil {
		t.Fatal(err)
	}
	if legacy.StatusCode != 404 {
		t.Fatalf("legacy status = %d, want 404", legacy.StatusCode)
	}
	v2, err := app.Test(httptest.NewRequest("GET", "/api/v2/admin/launcher-releases/", nil))
	if err != nil {
		t.Fatal(err)
	}
	if v2.StatusCode != 200 {
		t.Fatalf("v2 admin status = %d, want 200", v2.StatusCode)
	}
}

func TestV1BridgeDeadlineIsCheckedPerRequest(t *testing.T) {
	service := newTestService(t)
	app := fiber.New()
	auth := func(c fiber.Ctx) error {
		c.Locals("current-user", models.User{Login: "testadmin", Role: "admin"})
		return c.Next()
	}
	NewHandler(service, nil).RegisterRoutesWithV1BridgeUntil(app, auth, time.Now().UTC().Add(-time.Second))
	legacy, err := app.Test(httptest.NewRequest("GET", "/api/launcher/update?platform=linux-x64&version=0.1.0", nil))
	if err != nil {
		t.Fatal(err)
	}
	if legacy.StatusCode != 404 {
		t.Fatalf("expired legacy status = %d, want 404", legacy.StatusCode)
	}
	v2, err := app.Test(httptest.NewRequest("GET", "/api/v2/admin/launcher-releases/", nil))
	if err != nil || v2.StatusCode != 200 {
		t.Fatalf("v2 after bridge cutoff: status=%d err=%v", v2.StatusCode, err)
	}
}
