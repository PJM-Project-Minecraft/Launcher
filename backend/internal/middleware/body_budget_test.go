package middleware

import (
	"bufio"
	"bytes"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gofiber/fiber/v3"
)

func TestBodyBudgetRejectsContentLengthAndChunkedBodies(t *testing.T) {
	called := false
	app := fiber.New(fiber.Config{BodyLimit: 1024, StreamRequestBody: true})
	app.Use(BodyBudget(8, nil, nil))
	app.Post("/json", func(c fiber.Ctx) error {
		called = true
		return c.SendStatus(http.StatusNoContent)
	})
	req := httptest.NewRequest(http.MethodPost, "/json", bytes.NewReader(bytes.Repeat([]byte("x"), 9)))
	resp, err := app.Test(req)
	if err != nil {
		t.Fatalf("app.Test: %v", err)
	}
	if resp.StatusCode != http.StatusRequestEntityTooLarge || called {
		t.Fatalf("content-length request: status=%d handlerCalled=%v", resp.StatusCode, called)
	}
}

func TestBodyBudgetRejectsChunkedRawRequest(t *testing.T) {
	called := false
	app := fiber.New(fiber.Config{BodyLimit: 8, StreamRequestBody: true})
	app.Use(BodyBudget(8, nil, nil))
	app.Post("/json", func(c fiber.Ctx) error {
		called = true
		return c.SendStatus(http.StatusNoContent)
	})
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer app.Shutdown()
	go func() { _ = app.Listener(listener, fiber.ListenConfig{DisableStartupMessage: true}) }()

	conn, err := net.Dial("tcp", listener.Addr().String())
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.Close()
	_, _ = fmt.Fprint(conn, "POST /json HTTP/1.1\r\nHost: localhost\r\nTransfer-Encoding: chunked\r\nConnection: close\r\n\r\n9\r\nxxxxxxxxx\r\n0\r\n\r\n")
	resp, err := http.ReadResponse(bufio.NewReader(conn), &http.Request{Method: http.MethodPost})
	if err != nil {
		t.Fatalf("read response: %v", err)
	}
	if resp.StatusCode != http.StatusRequestEntityTooLarge || called {
		t.Fatalf("chunked request: status=%d handlerCalled=%v", resp.StatusCode, called)
	}
}

func TestBodyBudgetAllowsExplicitLargeRoute(t *testing.T) {
	app := fiber.New(fiber.Config{BodyLimit: 1024, StreamRequestBody: true})
	app.Use(BodyBudget(8, map[string]int{"/large/": 16}, nil))
	app.Post("/large/upload", func(c fiber.Ctx) error { return c.Send(c.Body()) })
	req := httptest.NewRequest(http.MethodPost, "/large/upload", bytes.NewReader(bytes.Repeat([]byte("x"), 12)))
	resp, err := app.Test(req)
	if err != nil {
		t.Fatalf("app.Test: %v", err)
	}
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
}
