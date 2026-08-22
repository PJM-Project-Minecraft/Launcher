package delivery

import (
	"context"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"launcher-backend/internal/models"

	"github.com/gofiber/fiber/v3"
)

func TestCreateDraftFromActiveViaAdminAPI(t *testing.T) {
	service, db := testService(t)
	profile := models.Profile{ID: newID(), Name: "Test", Slug: "test", Loader: "fabric", GameVersion: "1.21.1", JavaVersion: 21, IsActive: true}
	if err := db.Create(&profile).Error; err != nil {
		t.Fatal(err)
	}
	source := t.TempDir()
	if err := os.WriteFile(filepath.Join(source, "client.jar"), []byte("current client"), 0644); err != nil {
		t.Fatal(err)
	}
	if _, err := service.publishProfile(context.Background(), profile.ID, source, func(_, _ int) {}); err != nil {
		t.Fatal(err)
	}

	app := fiber.New()
	NewHandler(service).RegisterRoutes(app, func(c fiber.Ctx) error {
		c.Locals("current-user", models.User{Login: "testadmin", Role: "admin"})
		return c.Next()
	})
	response, err := app.Test(httptest.NewRequest(http.MethodPost, "/api/v2/admin/delivery/profiles/"+profile.ID+"/drafts/from-active", nil))
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusCreated {
		body, _ := io.ReadAll(response.Body)
		t.Fatalf("status=%d body=%s", response.StatusCode, body)
	}
	var body struct {
		Generation      string `json:"generation"`
		SFTPPath        string `json:"sftpPath"`
		SourceReleaseID string `json:"sourceReleaseId"`
		SeededFileCount int    `json:"seededFileCount"`
		SeededTotalSize int64  `json:"seededTotalSize"`
	}
	if err := json.NewDecoder(response.Body).Decode(&body); err != nil {
		t.Fatal(err)
	}
	if body.Generation == "" || body.SourceReleaseID == "" || body.SeededFileCount != 1 || body.SeededTotalSize != int64(len("current client")) {
		t.Fatalf("response = %+v", body)
	}
	if data, err := os.ReadFile(filepath.Join(body.SFTPPath, "client.jar")); err != nil || string(data) != "current client" {
		t.Fatalf("seeded client = %q, err=%v", data, err)
	}
}

func TestLauncherDescriptorIsSigned(t *testing.T) {
	service, db := testService(t)
	payload := []byte("signed-launcher-binary")
	digest := sha256.Sum256(payload)
	release := models.LauncherRelease{
		ID: newID(), Version: "1.2.3", IsActive: true,
		Files: []models.LauncherReleaseFile{{
			ID: newID(), Platform: "linux-x64", FileName: "launcher",
			HashSHA256: hex.EncodeToString(digest[:]), Size: int64(len(payload)),
			SignatureEd25519: strings.Repeat("a", 128),
		}},
	}
	release.Files[0].ReleaseID = release.ID
	path := filepath.Join(service.launcherRoot, release.Version, "linux-x64", "launcher")
	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, payload, 0755); err != nil {
		t.Fatal(err)
	}
	if err := db.Create(&release).Error; err != nil {
		t.Fatal(err)
	}
	if err := service.ImportLauncherRelease(t.Context(), release); err != nil {
		t.Fatal(err)
	}

	app := fiber.New()
	NewHandler(service).RegisterRoutes(app, func(c fiber.Ctx) error { return c.Next() })
	response, err := app.Test(httptest.NewRequest("GET", "/api/v2/launcher/releases/current?platform=linux-x64&from=1.0.0", nil))
	if err != nil {
		t.Fatal(err)
	}
	body, err := io.ReadAll(response.Body)
	if err != nil {
		t.Fatal(err)
	}
	signature, err := hex.DecodeString(response.Header.Get("X-Manifest-Signature"))
	if err != nil || len(signature) != ed25519.SignatureSize {
		t.Fatalf("signature header = %q", response.Header.Get("X-Manifest-Signature"))
	}
	if !ed25519.Verify(service.signingKey.Public().(ed25519.PublicKey), body, signature) {
		t.Fatal("launcher descriptor signature is invalid")
	}
	var descriptor LauncherManifest
	if err := json.Unmarshal(body, &descriptor); err != nil {
		t.Fatal(err)
	}
	if err := db.Model(&models.LauncherRelease{}).Where("id = ?", release.ID).Update("mandatory", true).Error; err != nil {
		t.Fatal(err)
	}
	updated, err := app.Test(httptest.NewRequest("GET", "/api/v2/launcher/releases/current?platform=linux-x64&from=1.0.0", nil))
	if err != nil {
		t.Fatal(err)
	}
	updatedBody, _ := io.ReadAll(updated.Body)
	if string(updatedBody) != string(body) || updated.Header.Get("X-Update-Mandatory") != "true" {
		t.Fatal("channel policy mutated the immutable launcher descriptor")
	}

	// Removing a release from the channel must not break a client which already
	// received its signed descriptor. Release-ID content lives until explicit GC.
	if err := db.Model(&models.LauncherRelease{}).Where("id = ?", release.ID).Update("is_active", false).Error; err != nil {
		t.Fatal(err)
	}
	chunkURL := "/api/v2/launcher/releases/" + release.ID + "/chunks/" + descriptor.Artifact.Chunks[0].SHA256
	chunkResponse, err := app.Test(httptest.NewRequest("GET", chunkURL, nil))
	if err != nil || chunkResponse.StatusCode != 200 {
		t.Fatalf("immutable chunk after deactivation: status=%d err=%v", chunkResponse.StatusCode, err)
	}
	artifactURL := "/api/v2/launcher/releases/" + release.ID + "/artifact?platform=linux-x64"
	artifactResponse, err := app.Test(httptest.NewRequest("GET", artifactURL, nil))
	if err != nil || artifactResponse.StatusCode != 200 {
		t.Fatalf("immutable artifact after deactivation: status=%d err=%v", artifactResponse.StatusCode, err)
	}
}
