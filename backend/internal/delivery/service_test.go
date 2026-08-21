package delivery

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"launcher-backend/internal/models"

	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
)

func testService(t *testing.T) (*Service, *gorm.DB) {
	t.Helper()
	db, err := gorm.Open(sqlite.Open(filepath.Join(t.TempDir(), "delivery.db")), &gorm.Config{})
	if err != nil {
		t.Fatal(err)
	}
	if err := db.AutoMigrate(
		&models.Profile{}, &models.DeliveryBlob{}, &models.ProfileRelease{},
		&models.ProfileReleaseFile{}, &models.ProfileReleaseFileChunk{},
		&models.DeliveryJob{}, &models.LauncherRelease{}, &models.LauncherReleaseFile{},
		&models.LauncherDeliveryArtifact{}, &models.LauncherDeliveryArtifactChunk{},
	); err != nil {
		t.Fatal(err)
	}
	seed := make([]byte, ed25519.SeedSize)
	if _, err := rand.Read(seed); err != nil {
		t.Fatal(err)
	}
	root := t.TempDir()
	service, err := NewService(db, filepath.Join(root, "delivery"), filepath.Join(root, "profiles"), filepath.Join(root, "launcher"), hex.EncodeToString(seed))
	if err != nil {
		t.Fatal(err)
	}
	return service, db
}

func TestChunkerIsDeterministicAndBounded(t *testing.T) {
	data := make([]byte, 19<<20)
	for index := range data {
		data[index] = byte(index*31 + index/97)
	}
	collect := func() []chunkData {
		result := make([]chunkData, 0)
		if err := splitChunks(bytes.NewReader(data), func(chunk chunkData) error { result = append(result, chunk); return nil }); err != nil {
			t.Fatal(err)
		}
		return result
	}
	left, right := collect(), collect()
	if len(left) < 3 || len(left) != len(right) {
		t.Fatalf("chunk counts: %d and %d", len(left), len(right))
	}
	var total int
	for index := range left {
		if left[index].Hash != right[index].Hash {
			t.Fatalf("chunk %d is not deterministic", index)
		}
		if len(left[index].Data) > chunkMax {
			t.Fatalf("chunk %d exceeds max", index)
		}
		if index < len(left)-1 && len(left[index].Data) < chunkMin {
			t.Fatalf("chunk %d below min", index)
		}
		total += len(left[index].Data)
	}
	if total != len(data) {
		t.Fatalf("chunk bytes = %d, want %d", total, len(data))
	}
}

func TestPublishProfileActivatesSignedImmutableRelease(t *testing.T) {
	service, db := testService(t)
	profile := models.Profile{ID: newID(), Name: "Test", Slug: "test", Loader: "neoforge", GameVersion: "1.21.1", JavaVersion: 21, IsActive: true}
	if err := db.Create(&profile).Error; err != nil {
		t.Fatal(err)
	}
	source := t.TempDir()
	if err := os.MkdirAll(filepath.Join(source, "mods"), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(source, "mods", "example.jar"), bytes.Repeat([]byte("content"), 300000), 0644); err != nil {
		t.Fatal(err)
	}
	releaseID, err := service.publishProfile(context.Background(), profile.ID, source, func(_, _ int) {})
	if err != nil {
		t.Fatal(err)
	}
	body, release, err := service.Manifest(context.Background(), profile.ID, releaseID)
	if err != nil {
		t.Fatal(err)
	}
	if !release.IsActive || release.ManifestSignature == "" {
		t.Fatalf("release = %+v", release)
	}
	signature, err := hex.DecodeString(release.ManifestSignature)
	if err != nil {
		t.Fatal(err)
	}
	if !ed25519.Verify(service.signingKey.Public().(ed25519.PublicKey), body, signature) {
		t.Fatal("manifest signature is invalid")
	}
	var manifest ProfileManifest
	if err := json.Unmarshal(body, &manifest); err != nil {
		t.Fatal(err)
	}
	if manifest.SchemaVersion != 2 || manifest.ReleaseID != releaseID || len(manifest.Files) != 1 || len(manifest.Files[0].Chunks) == 0 {
		t.Fatalf("manifest = %+v", manifest)
	}
	chunk := manifest.Files[0].Chunks[0]
	path, size, err := service.Blob(context.Background(), profile.ID, chunk.SHA256)
	if err != nil {
		t.Fatal(err)
	}
	if size != chunk.Size {
		t.Fatalf("blob size = %d, want %d", size, chunk.Size)
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatal(err)
	}
}

func TestPublishRepairsCorruptCASBlobWithSameSize(t *testing.T) {
	service, db := testService(t)
	profile := models.Profile{ID: newID(), Name: "Test", Slug: "test", Loader: "fabric", GameVersion: "1.21.1", JavaVersion: 21, IsActive: true}
	if err := db.Create(&profile).Error; err != nil {
		t.Fatal(err)
	}
	source := t.TempDir()
	payload := bytes.Repeat([]byte("trusted"), 300000)
	if err := os.WriteFile(filepath.Join(source, "client.jar"), payload, 0644); err != nil {
		t.Fatal(err)
	}
	releaseID, err := service.publishProfile(context.Background(), profile.ID, source, func(_, _ int) {})
	if err != nil {
		t.Fatal(err)
	}
	body, _, err := service.Manifest(context.Background(), profile.ID, releaseID)
	if err != nil {
		t.Fatal(err)
	}
	var manifest ProfileManifest
	if err := json.Unmarshal(body, &manifest); err != nil {
		t.Fatal(err)
	}
	chunk := manifest.Files[0].Chunks[0]
	if err := os.WriteFile(service.blobPath(chunk.SHA256), bytes.Repeat([]byte("x"), int(chunk.Size)), 0644); err != nil {
		t.Fatal(err)
	}
	if _, err := service.publishProfile(context.Background(), profile.ID, source, func(_, _ int) {}); err != nil {
		t.Fatal(err)
	}
	repaired, err := os.ReadFile(service.blobPath(chunk.SHA256))
	if err != nil {
		t.Fatal(err)
	}
	digest := sha256.Sum256(repaired)
	if hex.EncodeToString(digest[:]) != chunk.SHA256 {
		t.Fatal("corrupt CAS blob was not repaired")
	}
}

func TestReadyRenameIsTheOnlyPublicationSignal(t *testing.T) {
	service, db := testService(t)
	profile := models.Profile{ID: newID(), Name: "Test", Slug: "test", Loader: "fabric", GameVersion: "1.21.1", JavaVersion: 21, IsActive: true}
	if err := db.Create(&profile).Error; err != nil {
		t.Fatal(err)
	}
	generation, upload, err := service.CreateDraft(context.Background(), profile.ID)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(upload, "client.jar"), []byte("complete"), 0644); err != nil {
		t.Fatal(err)
	}
	watcher := NewWatcher(service, nil)
	watcher.reconcile()
	var count int64
	db.Model(&models.ProfileRelease{}).Count(&count)
	if count != 0 {
		t.Fatal(".upload directory was published")
	}
	ready := filepath.Join(filepath.Dir(upload), generation+".ready")
	if err := os.Rename(upload, ready); err != nil {
		t.Fatal(err)
	}
	watcher.reconcile()
	db.Model(&models.ProfileRelease{}).Count(&count)
	if count != 1 {
		t.Fatalf("release count = %d", count)
	}
}

func TestProcessingRecoveryFinalizesCommittedGenerationWithoutRepublish(t *testing.T) {
	service, db := testService(t)
	profile := models.Profile{ID: newID(), Name: "Test", Slug: "test", Loader: "fabric", GameVersion: "1.21.1", JavaVersion: 21, IsActive: true}
	if err := db.Create(&profile).Error; err != nil {
		t.Fatal(err)
	}
	generation, upload, err := service.CreateDraft(context.Background(), profile.ID)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(upload, "client.jar"), []byte("complete"), 0644); err != nil {
		t.Fatal(err)
	}
	processing := filepath.Join(filepath.Dir(upload), generation+".processing")
	if err := os.Rename(upload, processing); err != nil {
		t.Fatal(err)
	}
	var job models.DeliveryJob
	if err := db.Where("profile_id = ? AND generation = ?", profile.ID, generation).First(&job).Error; err != nil {
		t.Fatal(err)
	}
	if _, err := service.publishProfileForJob(context.Background(), profile.ID, processing, job.ID, generation, func(_, _ int) {}); err != nil {
		t.Fatal(err)
	}

	// Simulate a crash after the release transaction committed but before the
	// watcher marked the job succeeded and renamed .processing.
	NewWatcher(service, nil).reconcile()
	var releases int64
	db.Model(&models.ProfileRelease{}).Where("profile_id = ?", profile.ID).Count(&releases)
	if releases != 1 {
		t.Fatalf("release count = %d, want one idempotent publication", releases)
	}
	if err := db.First(&job, "id = ?", job.ID).Error; err != nil {
		t.Fatal(err)
	}
	if job.Status != "succeeded" || job.ReleaseID == nil {
		t.Fatalf("recovered job = %+v", job)
	}
	if _, err := os.Stat(filepath.Join(filepath.Dir(upload), generation+".published")); err != nil {
		t.Fatalf("published generation missing: %v", err)
	}
}

func TestLauncherJobResumesAndActivatesOutsideUploadRequest(t *testing.T) {
	service, db := testService(t)
	payload := []byte("launcher-worker-payload")
	digest := sha256.Sum256(payload)
	release := models.LauncherRelease{
		ID: newID(), Version: "2.0.0", IsActive: false,
		Files: []models.LauncherReleaseFile{{
			ID: newID(), Platform: "linux-x64", FileName: "launcher",
			HashSHA256: hex.EncodeToString(digest[:]), Size: int64(len(payload)), SignatureEd25519: strings.Repeat("a", 128),
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
	job, err := service.QueueLauncherRelease(context.Background(), release)
	if err != nil {
		t.Fatal(err)
	}
	if job.ProfileID != nil || job.ReleaseID != nil || job.Status != "queued" {
		t.Fatalf("queued launcher job = %+v", job)
	}
	// A restarted process turns running back into queued; either state is
	// recoverable by the same durable worker.
	if err := db.Model(&job).Update("status", "running").Error; err != nil {
		t.Fatal(err)
	}
	NewWatcher(service, nil).reconcileLauncherJobs()
	if err := db.First(&job, "id = ?", job.ID).Error; err != nil {
		t.Fatal(err)
	}
	if job.Status != "succeeded" || job.ReleaseID == nil {
		t.Fatalf("completed launcher job = %+v", job)
	}
	if err := db.First(&release, "id = ?", release.ID).Error; err != nil {
		t.Fatal(err)
	}
	if !release.IsActive || release.PublishedAt == nil {
		t.Fatalf("launcher release was not atomically activated: %+v", release)
	}
}

func TestFailedLauncherJobCanBeRequeued(t *testing.T) {
	service, db := testService(t)
	release := models.LauncherRelease{ID: newID(), Version: "2.1.0"}
	if err := db.Create(&release).Error; err != nil {
		t.Fatal(err)
	}
	job, err := service.QueueLauncherRelease(context.Background(), release)
	if err != nil {
		t.Fatal(err)
	}
	if err := db.Model(&job).Updates(map[string]any{"status": "failed", "error": "temporary"}).Error; err != nil {
		t.Fatal(err)
	}
	job, err = service.QueueLauncherRelease(context.Background(), release)
	if err != nil {
		t.Fatal(err)
	}
	if job.Status != "queued" || job.Error != "" {
		t.Fatalf("requeued job = %+v", job)
	}
}

func TestGarbageCollectRetainsNewestReleaseAndReferencedBlobs(t *testing.T) {
	service, db := testService(t)
	profile := models.Profile{ID: newID(), Name: "Test", Slug: "test", Loader: "neoforge", GameVersion: "1.21.1", JavaVersion: 21, IsActive: true}
	if err := db.Create(&profile).Error; err != nil {
		t.Fatal(err)
	}
	source := t.TempDir()
	file := filepath.Join(source, "client.jar")
	if err := os.WriteFile(file, bytes.Repeat([]byte("old"), 500000), 0644); err != nil {
		t.Fatal(err)
	}
	oldID, err := service.publishProfile(context.Background(), profile.ID, source, func(_, _ int) {})
	if err != nil {
		t.Fatal(err)
	}
	oldBody, _, err := service.Manifest(context.Background(), profile.ID, oldID)
	if err != nil {
		t.Fatal(err)
	}
	var oldManifest ProfileManifest
	if err := json.Unmarshal(oldBody, &oldManifest); err != nil {
		t.Fatal(err)
	}
	oldBlob := oldManifest.Files[0].Chunks[0].SHA256

	if err := os.WriteFile(file, bytes.Repeat([]byte("new"), 500000), 0644); err != nil {
		t.Fatal(err)
	}
	newID, err := service.publishProfile(context.Background(), profile.ID, source, func(_, _ int) {})
	if err != nil {
		t.Fatal(err)
	}
	result, err := service.GarbageCollect(context.Background(), 1, 0)
	if err != nil {
		t.Fatal(err)
	}
	if result.ProfileReleases != 1 || result.Blobs == 0 {
		var references int64
		db.Model(&models.ProfileReleaseFileChunk{}).Where("hash_sha256 = ?", oldBlob).Count(&references)
		var blob models.DeliveryBlob
		blobErr := db.First(&blob, "hash_sha256 = ?", oldBlob).Error
		t.Fatalf("gc result = %+v, old references=%d, old blob=%+v, blob err=%v", result, references, blob, blobErr)
	}
	if _, _, err := service.Manifest(context.Background(), profile.ID, oldID); !errors.Is(err, gorm.ErrRecordNotFound) {
		t.Fatalf("old release still exists: %v", err)
	}
	if _, _, err := service.Manifest(context.Background(), profile.ID, newID); err != nil {
		t.Fatalf("new release removed: %v", err)
	}
	if _, err := os.Stat(service.blobPath(oldBlob)); !os.IsNotExist(err) {
		t.Fatalf("old blob was not removed: %v", err)
	}
}

func TestGarbageCollectRemovesTombstonedLauncherSourceAfterGrace(t *testing.T) {
	service, db := testService(t)
	payload := []byte("old-launcher")
	digest := sha256.Sum256(payload)
	release := models.LauncherRelease{
		ID: newID(), Version: "3.0.0", IsActive: true,
		Files: []models.LauncherReleaseFile{{
			ID: newID(), Platform: "linux-x64", FileName: "launcher",
			HashSHA256: hex.EncodeToString(digest[:]), Size: int64(len(payload)), SignatureEd25519: strings.Repeat("b", 128),
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
	if err := service.ImportLauncherRelease(context.Background(), release); err != nil {
		t.Fatal(err)
	}
	old := time.Now().UTC().Add(-48 * time.Hour)
	if err := db.Model(&models.LauncherRelease{}).Where("id = ?", release.ID).Updates(map[string]any{"is_active": false, "published_at": old}).Error; err != nil {
		t.Fatal(err)
	}
	if err := db.Model(&models.LauncherDeliveryArtifact{}).Where("release_id = ?", release.ID).Update("created_at", old).Error; err != nil {
		t.Fatal(err)
	}
	result, err := service.GarbageCollect(context.Background(), 1, 24*time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if result.LauncherReleases != 1 || result.LauncherArtifacts != 1 {
		t.Fatalf("launcher GC result = %+v", result)
	}
	if _, err := os.Stat(filepath.Join(service.launcherRoot, release.Version)); !os.IsNotExist(err) {
		t.Fatalf("launcher source survived GC: %v", err)
	}
	if err := db.First(&models.LauncherRelease{}, "id = ?", release.ID).Error; !errors.Is(err, gorm.ErrRecordNotFound) {
		t.Fatalf("launcher metadata survived GC: %v", err)
	}
}
