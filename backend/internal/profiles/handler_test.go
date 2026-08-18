package profiles

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"net"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"launcher-backend/internal/events"

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
