package yggdrasil

import (
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"launcher-backend/internal/models"

	"github.com/gofiber/fiber/v3"
)

type limitedEntropyReader struct {
	remaining int
}

func (r *limitedEntropyReader) Read(p []byte) (int, error) {
	if r.remaining == 0 {
		return 0, errors.New("entropy unavailable")
	}
	n := len(p)
	if n > r.remaining {
		n = r.remaining
	}
	for i := 0; i < n; i++ {
		p[i] = byte(i + 1)
	}
	r.remaining -= n
	if n < len(p) {
		return n, io.ErrUnexpectedEOF
	}
	return n, nil
}

func TestIssueSessionEntropyFailureDoesNotStoreSession(t *testing.T) {
	for name, available := range map[string]int{
		"first token":  0,
		"second token": 16,
	} {
		t.Run(name, func(t *testing.T) {
			svc := newTestService()
			svc.entropy = &limitedEntropyReader{remaining: available}
			if _, err := svc.IssueSession(models.User{Login: "Liko", ProviderUUID: "u"}, "nonce-fail"); err == nil {
				t.Fatal("entropy failure must abort session issuance")
			}
			if _, ok := svc.Store().SessionByNonce("nonce-fail"); ok {
				t.Fatal("partial session was stored after entropy failure")
			}
		})
	}
}

func TestRefreshEntropyFailurePreservesExistingSession(t *testing.T) {
	svc := newTestService()
	sess, err := svc.IssueSession(models.User{Login: "Liko", ProviderUUID: "u"}, "nonce-refresh")
	if err != nil {
		t.Fatalf("issue session: %v", err)
	}
	svc.entropy = &limitedEntropyReader{}

	app := fiber.New()
	NewHandler(svc).RegisterRoutes(app, func(c fiber.Ctx) error { return c.Next() })
	req := httptest.NewRequest(http.MethodPost, "/api/yggdrasil/authserver/refresh",
		strings.NewReader(`{"accessToken":"`+sess.AccessToken+`"}`))
	req.Header.Set("Content-Type", "application/json")
	resp, err := app.Test(req)
	if err != nil {
		t.Fatalf("app.Test: %v", err)
	}
	if resp.StatusCode != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500", resp.StatusCode)
	}
	if _, ok := svc.Store().Session(sess.AccessToken); !ok {
		t.Fatal("old access token was mutated after entropy failure")
	}
}
