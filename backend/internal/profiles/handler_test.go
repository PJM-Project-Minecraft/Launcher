package profiles

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"launcher-backend/internal/events"
	"launcher-backend/internal/models"

	fastws "github.com/fasthttp/websocket"
	"github.com/gofiber/fiber/v3"
)

func TestBundleDownloadSupportsRangeAndETag(t *testing.T) {
	service := newTestService(t)
	profile, err := service.Create(context.Background(), ProfileRequest{Name: "Bundle", Slug: "bundle", Loader: "fabric", GameVersion: "1.21.1"})
	if err != nil {
		t.Fatal(err)
	}
	writeTestFile(t, filepath.Join(service.storageRoot, profile.Slug, "files", "mods", "a.jar"), strings.Repeat("bundle-data", 100))
	if _, err := service.Scan(context.Background(), profile.ID); err != nil {
		t.Fatal(err)
	}

	app := fiber.New()
	NewHandler(service, nil).RegisterRoutes(app, func(c fiber.Ctx) error { return c.Next() })
	req := httptest.NewRequest("GET", fmt.Sprintf("/api/profiles/%s/bundles/1", profile.ID), nil)
	req.Header.Set("Range", "bytes=0-31")
	res, err := app.Test(req)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	body, _ := io.ReadAll(res.Body)
	if res.StatusCode != 206 || len(body) != 32 {
		t.Fatalf("range response status=%d len=%d", res.StatusCode, len(body))
	}
	if res.Header.Get("ETag") == "" || res.Header.Get("Accept-Ranges") != "bytes" {
		t.Fatalf("missing bundle cache/range headers: %v", res.Header)
	}
}

func TestManifestSupportsConditionalRequest(t *testing.T) {
	service := newTestService(t)
	profile, err := service.Create(context.Background(), ProfileRequest{
		Name: "Conditional", Slug: "conditional", Loader: "fabric", GameVersion: "1.21.1",
	})
	if err != nil {
		t.Fatal(err)
	}
	writeTestFile(t, filepath.Join(service.storageRoot, profile.Slug, "files", "mods", "a.jar"), "data")
	if _, err := service.Scan(context.Background(), profile.ID); err != nil {
		t.Fatal(err)
	}

	app := fiber.New()
	NewHandler(service, nil).RegisterRoutes(app, func(c fiber.Ctx) error { return c.Next() })
	path := fmt.Sprintf("/api/profiles/%s/manifest", profile.ID)
	first, err := app.Test(httptest.NewRequest("GET", path, nil))
	if err != nil {
		t.Fatal(err)
	}
	first.Body.Close()
	etag := first.Header.Get("ETag")
	if first.StatusCode != 200 || etag == "" {
		t.Fatalf("first manifest status=%d etag=%q", first.StatusCode, etag)
	}

	req := httptest.NewRequest("GET", path, nil)
	req.Header.Set("If-None-Match", etag)
	second, err := app.Test(req)
	if err != nil {
		t.Fatal(err)
	}
	defer second.Body.Close()
	if second.StatusCode != 304 {
		t.Fatalf("conditional manifest status=%d, want 304", second.StatusCode)
	}

	active := true
	if _, err := service.Update(context.Background(), profile.ID, ProfileRequest{
		Name: "Conditional", Slug: "conditional", Loader: "fabric", GameVersion: "1.21.1",
		JVMArgs: "-Xmx6G", IsActive: &active,
	}); err != nil {
		t.Fatal(err)
	}
	stale := httptest.NewRequest("GET", path, nil)
	stale.Header.Set("If-None-Match", etag)
	third, err := app.Test(stale)
	if err != nil {
		t.Fatal(err)
	}
	defer third.Body.Close()
	if third.StatusCode != 200 || third.Header.Get("ETag") == etag {
		t.Fatalf("profile update did not invalidate manifest etag: status=%d etag=%q", third.StatusCode, third.Header.Get("ETag"))
	}
}

func TestAdminWebSocketUsesSingleUseTicketAndSendsSnapshot(t *testing.T) {
	service := newTestService(t)
	handler := NewHandler(service, events.NewBroker())
	app := fiber.New()
	// В production общий /api/admin middleware регистрируется до profiles.
	// WebSocket обязан жить вне этого prefix, иначе upgrade без Authorization
	// будет отклонён раньше проверки одноразового билета.
	admin := app.Group("/api/admin")
	admin.Use(func(c fiber.Ctx) error {
		return c.Status(http.StatusUnauthorized).JSON(fiber.Map{"message": "Требуется авторизация"})
	})
	publicProfiles := app.Group("/api/profiles")
	publicProfiles.Use(func(c fiber.Ctx) error {
		return c.Status(http.StatusUnauthorized).JSON(fiber.Map{"message": "Требуется авторизация"})
	})
	handler.RegisterRoutes(app, func(c fiber.Ctx) error { return c.Next() })

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	go func() { _ = app.Listener(ln, fiber.ListenConfig{DisableStartupMessage: true}) }()
	defer ln.Close()

	ticket, _, err := handler.tickets.Issue()
	if err != nil {
		t.Fatal(err)
	}
	url := fmt.Sprintf("ws://%s/api/realtime/profiles/ws?ticket=%s", ln.Addr(), ticket)
	conn, _, err := fastws.DefaultDialer.Dial(url, nil)
	if err != nil {
		t.Fatalf("websocket dial: %v", err)
	}
	defer conn.Close()
	_ = conn.SetReadDeadline(time.Now().Add(3 * time.Second))
	_, payload, err := conn.ReadMessage()
	if err != nil {
		t.Fatalf("read initial snapshot: %v", err)
	}
	var envelope socketEnvelope
	if err := json.Unmarshal(payload, &envelope); err != nil || envelope.Type != "manifest.snapshot" {
		t.Fatalf("initial websocket payload = %s, err=%v", payload, err)
	}

	second, response, err := fastws.DefaultDialer.Dial(url, nil)
	if second != nil {
		second.Close()
	}
	if err == nil || response == nil || response.StatusCode != http.StatusUnauthorized {
		t.Fatalf("reused ticket response=%v err=%v", response, err)
	}
}

func TestAdminBuildSnapshotsRestoreProgressAfterPageReload(t *testing.T) {
	service := newTestService(t)
	profile, err := service.Create(context.Background(), ProfileRequest{
		Name: "Reload", Slug: "reload", Loader: "fabric", GameVersion: "1.21.1",
	})
	if err != nil {
		t.Fatal(err)
	}
	writeTestFile(t, filepath.Join(service.storageRoot, profile.Slug, "files", "mods", "a.jar"), "data")

	handler := NewHandler(service, nil)
	// Держим worker занятым: HTTP-гидратация должна вернуть именно активную
	// queued-задачу, как при обновлении страницы посреди публикации.
	handler.builds.worker <- struct{}{}
	defer func() { <-handler.builds.worker }()
	started, created, err := handler.builds.Start(context.Background(), profile.ID)
	if err != nil || !created {
		t.Fatalf("Start() created=%v err=%v", created, err)
	}

	app := fiber.New()
	handler.RegisterRoutes(app, func(c fiber.Ctx) error {
		c.Locals("current-user", models.User{Login: "testadmin", Role: "admin"})
		return c.Next()
	})
	response, err := app.Test(httptest.NewRequest("GET", "/api/admin/profiles/builds", nil))
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		t.Fatalf("build snapshots status=%d, want 200", response.StatusCode)
	}
	var snapshots []BuildSnapshot
	if err := json.NewDecoder(response.Body).Decode(&snapshots); err != nil {
		t.Fatal(err)
	}
	if len(snapshots) != 1 || snapshots[0].ID != started.ID || snapshots[0].Status != BuildQueued {
		t.Fatalf("build snapshots = %+v, want queued %s", snapshots, started.ID)
	}
}

// TestEventsStreamDeliversProfileChange поднимает реальный Fiber-сервер и через
// сырое TCP-соединение проверяет, что SSE-эндпоинт /api/profiles/events:
//   - отдаёт заголовок text/event-stream (статический /events не перехвачен /:id);
//   - доставляет событие "profiles" сразу после публикации в брокере (real-time).
//
// Сырой сокет используется намеренно: net/http-клиент Go буферизует chunked-поток
// и отдаёт его не сразу, что искажает замер задержки доставки.
func TestEventsStreamDeliversProfileChange(t *testing.T) {
	broker := events.NewBroker()
	handler := NewHandler(Service{}, broker)

	app := fiber.New()
	passthrough := func(c fiber.Ctx) error { return c.Next() }
	handler.RegisterRoutes(app, passthrough)

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	go func() {
		_ = app.Listener(ln, fiber.ListenConfig{DisableStartupMessage: true})
	}()
	defer ln.Close()

	conn, err := net.Dial("tcp", ln.Addr().String())
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.Close()
	if _, err := fmt.Fprintf(conn, "GET /api/profiles/events HTTP/1.1\r\nHost: localhost\r\n\r\n"); err != nil {
		t.Fatalf("write request: %v", err)
	}

	// Момент подписки SSE-обработчика на брокер гонится с установкой соединения,
	// поэтому публикуем периодически, пока событие не дойдёт.
	stop := make(chan struct{})
	defer close(stop)
	go func() {
		ticker := time.NewTicker(50 * time.Millisecond)
		defer ticker.Stop()
		for {
			select {
			case <-stop:
				return
			case <-ticker.C:
				broker.Publish("profiles")
			}
		}
	}()

	_ = conn.SetReadDeadline(time.Now().Add(5 * time.Second))
	reader := bufio.NewReader(conn)

	var sawEventStream, sawData bool
	for !sawData {
		line, err := reader.ReadString('\n')
		if err != nil {
			t.Fatalf("read SSE stream: %v (saw event-stream=%v)", err, sawEventStream)
		}
		switch {
		case strings.HasPrefix(line, "Content-Type:") && strings.Contains(line, "text/event-stream"):
			sawEventStream = true
		case strings.HasPrefix(line, "data:") && strings.Contains(line, "profiles"):
			sawData = true
		}
	}

	if !sawEventStream {
		t.Fatal("missing Content-Type: text/event-stream header")
	}
}
