package delivery

import (
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"launcher-backend/internal/models"

	"github.com/gofiber/fiber/v3"
)

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
