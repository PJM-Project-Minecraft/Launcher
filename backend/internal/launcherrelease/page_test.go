package launcherrelease

import (
	"bytes"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gofiber/fiber/v3"
)

// uploadRelease заливает релиз с бинарниками под указанные платформы через
// admin-ручку (в newTestApp она открыта passthrough-мидлварой).
func uploadRelease(t *testing.T, app *fiber.App, version string, platforms ...string) {
	t.Helper()
	body := &bytes.Buffer{}
	writer := multipart.NewWriter(body)
	_ = writer.WriteField("version", version)
	for _, p := range platforms {
		part, _ := writer.CreateFormFile(p, "launcher-"+p)
		_, _ = part.Write([]byte("fake-binary-" + p))
	}
	_ = writer.Close()
	req := httptest.NewRequest("POST", "/api/admin/releases/", body)
	req.Header.Set("Content-Type", writer.FormDataContentType())
	res, err := app.Test(req)
	if err != nil {
		t.Fatalf("upload release: %v", err)
	}
	if res.StatusCode != 201 {
		raw, _ := io.ReadAll(res.Body)
		t.Fatalf("upload status = %d: %s", res.StatusCode, raw)
	}
}

func readBody(t *testing.T, res *http.Response) string {
	t.Helper()
	raw, err := io.ReadAll(res.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	return string(raw)
}

func TestDownloadPageEmpty(t *testing.T) {
	app, _, _ := newTestApp(t)
	res, err := app.Test(httptest.NewRequest("GET", "/download", nil))
	if err != nil {
		t.Fatalf("app.Test: %v", err)
	}
	if res.StatusCode != 200 {
		t.Fatalf("status = %d, want 200", res.StatusCode)
	}
	body := readBody(t, res)
	if !strings.Contains(body, "готовятся") {
		t.Errorf("пустая витрина должна сообщать что сборки готовятся; got:\n%s", body)
	}
	if strings.Contains(body, "Скачать для") {
		t.Errorf("без релизов не должно быть кнопок скачивания")
	}
}

func TestDownloadPageWithReleases(t *testing.T) {
	app, _, _ := newTestApp(t)
	uploadRelease(t, app, "0.3.8", "linux-x64", "windows-x64")

	res, err := app.Test(httptest.NewRequest("GET", "/download", nil))
	if err != nil {
		t.Fatalf("app.Test: %v", err)
	}
	body := readBody(t, res)
	for _, want := range []string{
		"Скачать для Linux", "Скачать для Windows", "v0.3.8",
		"/api/launcher/download/0.3.8/linux-x64",
		"/api/launcher/download/0.3.8/windows-x64",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("витрина не содержит %q", want)
		}
	}
}

func TestDownloadPageDetectsWindows(t *testing.T) {
	app, _, _ := newTestApp(t)
	uploadRelease(t, app, "0.3.8", "linux-x64", "windows-x64")

	req := httptest.NewRequest("GET", "/download", nil)
	req.Header.Set("User-Agent", "Mozilla/5.0 (Windows NT 10.0; Win64; x64)")
	res, err := app.Test(req)
	if err != nil {
		t.Fatalf("app.Test: %v", err)
	}
	body := readBody(t, res)
	if !strings.Contains(body, "у вас Windows") {
		t.Errorf("для Windows-UA должна быть подсказка про Windows")
	}
	// primary-карточка (Windows) должна идти раньше Linux-карточки.
	win := strings.Index(body, "Скачать для Windows")
	lin := strings.Index(body, "Скачать для Linux")
	if win < 0 || lin < 0 || win > lin {
		t.Errorf("Windows-карточка должна быть первой при Windows-UA (win=%d lin=%d)", win, lin)
	}
}

func TestDownloadLogo(t *testing.T) {
	app, _, _ := newTestApp(t)
	res, err := app.Test(httptest.NewRequest("GET", "/download/pjm.png", nil))
	if err != nil {
		t.Fatalf("app.Test: %v", err)
	}
	if res.StatusCode != 200 {
		t.Fatalf("status = %d, want 200", res.StatusCode)
	}
	if ct := res.Header.Get("Content-Type"); ct != "image/png" {
		t.Errorf("Content-Type = %q, want image/png", ct)
	}
}

func TestDetectPlatform(t *testing.T) {
	cases := map[string]string{
		"Mozilla/5.0 (Windows NT 10.0; Win64; x64)":         "windows-x64",
		"Mozilla/5.0 (X11; Linux x86_64)":                   "linux-x64",
		"Mozilla/5.0 (X11; Ubuntu; Linux x86_64; rv:120.0)": "linux-x64",
		"Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7)":   "",
		"Mozilla/5.0 (Linux; Android 13; Pixel 7)":          "", // Android — не десктоп
		"": "",
	}
	for ua, want := range cases {
		if got := detectPlatform(ua); got != want {
			t.Errorf("detectPlatform(%q) = %q, want %q", ua, got, want)
		}
	}
}

func TestHumanSize(t *testing.T) {
	cases := map[int64]string{
		0:          "",
		-5:         "",
		5_500_000:  "5.5 МБ",
		26_832_384: "27 МБ",
	}
	for n, want := range cases {
		if got := humanSize(n); got != want {
			t.Errorf("humanSize(%d) = %q, want %q", n, got, want)
		}
	}
}
