package middleware

import (
	"compress/gzip"
	"io"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gofiber/fiber/v3"
)

func TestManifestCompressionNegotiatesGzip(t *testing.T) {
	t.Parallel()

	payload := `{"files":[` + strings.Repeat(`{"path":"assets/example.json","hashSha256":"0123456789abcdef"},`, 2_000) + `{}]}`
	app := fiber.New()
	app.Use(ManifestCompression())
	app.Get("/api/profiles/profile-id/manifest", func(c fiber.Ctx) error {
		c.Type("json")
		return c.SendString(payload)
	})

	req := httptest.NewRequest("GET", "/api/profiles/profile-id/manifest", nil)
	req.Header.Set("Accept-Encoding", "gzip")
	resp, err := app.Test(req)
	if err != nil {
		t.Fatalf("request manifest: %v", err)
	}
	defer resp.Body.Close()
	if got := resp.Header.Get("Content-Encoding"); got != "gzip" {
		t.Fatalf("Content-Encoding = %q, want gzip", got)
	}
	compressed, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read compressed manifest: %v", err)
	}
	if len(compressed) >= len(payload)/2 {
		t.Fatalf("compressed manifest is unexpectedly large: %d of %d bytes", len(compressed), len(payload))
	}

	reader, err := gzip.NewReader(strings.NewReader(string(compressed)))
	if err != nil {
		t.Fatalf("open gzip response: %v", err)
	}
	decoded, err := io.ReadAll(reader)
	if err != nil {
		t.Fatalf("decode gzip response: %v", err)
	}
	if err := reader.Close(); err != nil {
		t.Fatalf("close gzip response: %v", err)
	}
	if string(decoded) != payload {
		t.Fatal("decoded manifest differs from response payload")
	}
}

func TestManifestCompressionSkipsImmutableObjects(t *testing.T) {
	t.Parallel()

	payload := strings.Repeat("already-compressed-binary", 1_000)
	app := fiber.New()
	app.Use(ManifestCompression())
	app.Get("/api/profiles/profile-id/objects/hash", func(c fiber.Ctx) error {
		return c.SendString(payload)
	})

	req := httptest.NewRequest("GET", "/api/profiles/profile-id/objects/hash", nil)
	req.Header.Set("Accept-Encoding", "gzip")
	resp, err := app.Test(req)
	if err != nil {
		t.Fatalf("request object: %v", err)
	}
	defer resp.Body.Close()
	if got := resp.Header.Get("Content-Encoding"); got != "" {
		t.Fatalf("Content-Encoding = %q, want identity", got)
	}
	decoded, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read object response: %v", err)
	}
	if string(decoded) != payload {
		t.Fatal("object response was transformed")
	}
}

func TestManifestCompressionKeepsLegacyClientsOnIdentity(t *testing.T) {
	t.Parallel()

	payload := strings.Repeat("manifest-json", 1_000)
	app := fiber.New()
	app.Use(ManifestCompression())
	app.Get("/api/profiles/profile-id/manifest", func(c fiber.Ctx) error {
		return c.SendString(payload)
	})

	resp, err := app.Test(httptest.NewRequest("GET", "/api/profiles/profile-id/manifest", nil))
	if err != nil {
		t.Fatalf("request manifest: %v", err)
	}
	defer resp.Body.Close()
	if got := resp.Header.Get("Content-Encoding"); got != "" {
		t.Fatalf("Content-Encoding = %q, want identity", got)
	}
	decoded, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read manifest response: %v", err)
	}
	if string(decoded) != payload {
		t.Fatal("legacy manifest response was transformed")
	}
}
