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

	"launcher-backend/internal/events"
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
	if !pathMatchesPreserve("tacz/runtime-owned-pack/file.json", manifest.Profile.PreservePaths) {
		t.Fatalf("runtime-owned tacz path missing from preserve paths: %v", manifest.Profile.PreservePaths)
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

func TestPublishProfileRejectsManagedFilesInsidePreservePaths(t *testing.T) {
	tests := []struct {
		name         string
		preservePath string
		managedPath  string
	}{
		{name: "directory descendant", preservePath: ".voxy/", managedPath: ".voxy/saves/cache.db"},
		{name: "windows case folding", preservePath: ".voxy/", managedPath: ".VOXY/saves/cache.db"},
		{name: "managed descendant of preserved file", preservePath: "options.txt", managedPath: "options.txt/child"},
		{name: "managed ancestor of preserved file", preservePath: "config/player.json", managedPath: "config"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			service, db := testService(t)
			profile := models.Profile{
				ID: newID(), Name: "Test", Slug: "test", Loader: "fabric",
				GameVersion: "1.21.1", JavaVersion: 21, IsActive: true,
				PreservePaths: []string{test.preservePath},
			}
			if err := db.Create(&profile).Error; err != nil {
				t.Fatal(err)
			}
			source := t.TempDir()
			managed := filepath.Join(source, filepath.FromSlash(test.managedPath))
			if err := os.MkdirAll(filepath.Dir(managed), 0755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(managed, []byte("managed collision"), 0644); err != nil {
				t.Fatal(err)
			}

			_, err := service.publishProfile(context.Background(), profile.ID, source, func(_, _ int) {})
			if err == nil || !strings.Contains(err.Error(), test.preservePath) {
				t.Fatalf("publish error = %v, want preserve-path collision", err)
			}
			var releases int64
			if err := db.Model(&models.ProfileRelease{}).Where("profile_id = ?", profile.ID).Count(&releases).Error; err != nil {
				t.Fatal(err)
			}
			if releases != 0 {
				t.Fatalf("published releases = %d, want 0", releases)
			}
		})
	}
}

func TestCreateDraftFromActiveMaterializesManagedFilesAndSkipsCurrentPreservePaths(t *testing.T) {
	service, db := testService(t)
	profile := models.Profile{
		ID: newID(), Name: "Test", Slug: "test", Loader: "fabric",
		GameVersion: "1.21.1", JavaVersion: 21, IsActive: true,
		PreservePaths: []string{"options.txt"},
	}
	if err := db.Create(&profile).Error; err != nil {
		t.Fatal(err)
	}
	source := t.TempDir()
	if err := os.MkdirAll(filepath.Join(source, "mods"), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(source, ".voxy", "saves"), 0755); err != nil {
		t.Fatal(err)
	}
	modPayload := []byte("managed mod")
	if err := os.WriteFile(filepath.Join(source, "mods", "current.jar"), modPayload, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(source, ".voxy", "saves", "cache.db"), []byte("old cache"), 0644); err != nil {
		t.Fatal(err)
	}
	releaseID, err := service.publishProfile(context.Background(), profile.ID, source, func(_, _ int) {})
	if err != nil {
		t.Fatal(err)
	}
	profile.PreservePaths = []string{".voxy/"}
	if err := db.Save(&profile).Error; err != nil {
		t.Fatal(err)
	}

	draft, err := service.CreateDraftFromActive(context.Background(), profile.ID)
	if err != nil {
		t.Fatal(err)
	}
	if draft.SourceReleaseID != releaseID || draft.SeededFiles != 1 || draft.SeededSize != int64(len(modPayload)) {
		t.Fatalf("seeded draft = %+v", draft)
	}
	seededMod := filepath.Join(draft.Path, "mods", "current.jar")
	data, err := os.ReadFile(seededMod)
	if err != nil || !bytes.Equal(data, modPayload) {
		t.Fatalf("seeded mod = %q, err=%v", data, err)
	}
	if info, err := os.Stat(seededMod); err != nil || info.Mode().Perm()&0111 == 0 {
		t.Fatalf("seeded executable mode = %v, err=%v", info, err)
	}
	if _, err := os.Stat(filepath.Join(draft.Path, ".voxy")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("preserved .voxy was copied into draft: %v", err)
	}
	var job models.DeliveryJob
	if err := db.Where("profile_id = ? AND generation = ?", profile.ID, draft.Generation).First(&job).Error; err != nil {
		t.Fatal(err)
	}
	if job.Status != "waiting" || job.Phase != "upload" {
		t.Fatalf("draft job = %+v", job)
	}

	if err := os.WriteFile(seededMod, []byte("replacement"), 0644); err != nil {
		t.Fatal(err)
	}
	second, err := service.CreateDraftFromActive(context.Background(), profile.ID)
	if err != nil {
		t.Fatal(err)
	}
	secondData, err := os.ReadFile(filepath.Join(second.Path, "mods", "current.jar"))
	if err != nil || !bytes.Equal(secondData, modPayload) {
		t.Fatalf("second seeded mod = %q, err=%v; first draft mutated immutable source", secondData, err)
	}
}

func TestCreateDraftFromActivePersistsFailureBeforeMaterialization(t *testing.T) {
	service, db := testService(t)
	profile := models.Profile{ID: newID(), Name: "Test", Slug: "test", Loader: "fabric", GameVersion: "1.21.1", JavaVersion: 21, IsActive: true}
	if err := db.Create(&profile).Error; err != nil {
		t.Fatal(err)
	}
	source := t.TempDir()
	if err := os.WriteFile(filepath.Join(source, "client.jar"), []byte("current client"), 0644); err != nil {
		t.Fatal(err)
	}
	releaseID, err := service.publishProfile(context.Background(), profile.ID, source, func(_, _ int) {})
	if err != nil {
		t.Fatal(err)
	}
	var chunk models.ProfileReleaseFileChunk
	if err := db.Joins("JOIN profile_release_files ON profile_release_files.id = profile_release_file_chunks.file_id").
		Where("profile_release_files.release_id = ?", releaseID).First(&chunk).Error; err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(service.blobPath(chunk.HashSHA256)); err != nil {
		t.Fatal(err)
	}

	if _, err := service.CreateDraftFromActive(context.Background(), profile.ID); err == nil {
		t.Fatal("CreateDraftFromActive succeeded with a missing CAS chunk")
	}
	var job models.DeliveryJob
	if err := db.Where("profile_id = ? AND source_release_id = ?", profile.ID, releaseID).First(&job).Error; err != nil {
		t.Fatalf("durable seed job missing: %v", err)
	}
	if job.Status != "failed" || job.Phase != "failed" || job.EndedAt == nil || job.Error == "" {
		t.Fatalf("failed seed job = %+v", job)
	}
	if _, err := os.Stat(filepath.Join(service.incomingRoot(), profile.ID, job.Generation+".upload")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("failed seed directory was not removed: %v", err)
	}
}

func TestWatcherRecoversInterruptedSeedDraft(t *testing.T) {
	service, db := testService(t)
	profile := models.Profile{ID: newID(), Name: "Test", Slug: "test", Loader: "fabric", GameVersion: "1.21.1", JavaVersion: 21, IsActive: true}
	if err := db.Create(&profile).Error; err != nil {
		t.Fatal(err)
	}
	source := t.TempDir()
	payload := []byte("current client")
	if err := os.WriteFile(filepath.Join(source, "client.jar"), payload, 0644); err != nil {
		t.Fatal(err)
	}
	releaseID, err := service.publishProfile(context.Background(), profile.ID, source, func(_, _ int) {})
	if err != nil {
		t.Fatal(err)
	}
	generation := "seed-recovery-" + newID()
	started := time.Now().UTC().Add(-time.Minute)
	job := models.DeliveryJob{
		ID: newID(), Kind: "profile", ProfileID: &profile.ID, Generation: generation,
		Status: "queued", Phase: "recovery", Message: "resume", SourceReleaseID: &releaseID, StartedAt: &started,
	}
	if err := db.Create(&job).Error; err != nil {
		t.Fatal(err)
	}
	upload := filepath.Join(service.incomingRoot(), profile.ID, generation+".upload")
	if err := os.MkdirAll(upload, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(upload, "partial"), []byte("partial"), 0644); err != nil {
		t.Fatal(err)
	}

	NewWatcher(service, nil).reconcile()
	if err := db.First(&job, "id = ?", job.ID).Error; err != nil {
		t.Fatal(err)
	}
	if job.Status != "waiting" || job.Phase != "upload" || job.SourceReleaseID != nil {
		t.Fatalf("recovered seed job = %+v", job)
	}
	data, err := os.ReadFile(filepath.Join(upload, "client.jar"))
	if err != nil || !bytes.Equal(data, payload) {
		t.Fatalf("recovered client = %q, err=%v", data, err)
	}
	if _, err := os.Stat(filepath.Join(upload, "partial")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("partial seed survived recovery: %v", err)
	}
}

func TestWatcherRejectsReadyWhileSeedJobIsStillRunning(t *testing.T) {
	service, db := testService(t)
	profile := models.Profile{ID: newID(), Name: "Test", Slug: "test", Loader: "fabric", GameVersion: "1.21.1", JavaVersion: 21, IsActive: true}
	if err := db.Create(&profile).Error; err != nil {
		t.Fatal(err)
	}
	releaseID := newID()
	generation := "early-ready-" + newID()
	job := models.DeliveryJob{
		ID: newID(), Kind: "profile", ProfileID: &profile.ID, Generation: generation,
		Status: "running", Phase: "seeding", Message: "seeding", SourceReleaseID: &releaseID,
	}
	if err := db.Create(&job).Error; err != nil {
		t.Fatal(err)
	}
	ready := filepath.Join(service.incomingRoot(), profile.ID, generation+".ready")
	if err := os.MkdirAll(ready, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(ready, "partial.jar"), []byte("partial"), 0644); err != nil {
		t.Fatal(err)
	}

	NewWatcher(service, nil).reconcileProfiles()
	var releases int64
	if err := db.Model(&models.ProfileRelease{}).Where("profile_id = ?", profile.ID).Count(&releases).Error; err != nil {
		t.Fatal(err)
	}
	if releases != 0 {
		t.Fatalf("published releases = %d, want 0", releases)
	}
	if _, err := os.Stat(ready); err != nil {
		t.Fatalf("early ready was claimed: %v", err)
	}
}

func TestWatcherRejectsReadyCreatedBesideUpload(t *testing.T) {
	service, db := testService(t)
	profile := models.Profile{ID: newID(), Name: "Test", Slug: "test", Loader: "fabric", GameVersion: "1.21.1", JavaVersion: 21, IsActive: true}
	if err := db.Create(&profile).Error; err != nil {
		t.Fatal(err)
	}
	generation := "fake-ready-" + newID()
	job := models.DeliveryJob{
		ID: newID(), Kind: "profile", ProfileID: &profile.ID, Generation: generation,
		Status: "waiting", Phase: "upload", Message: "upload",
	}
	if err := db.Create(&job).Error; err != nil {
		t.Fatal(err)
	}
	root := filepath.Join(service.incomingRoot(), profile.ID)
	upload := filepath.Join(root, generation+".upload")
	ready := filepath.Join(root, generation+".ready")
	if err := os.MkdirAll(upload, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(ready, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(ready, "partial.jar"), []byte("partial"), 0644); err != nil {
		t.Fatal(err)
	}

	NewWatcher(service, nil).reconcileProfiles()
	var releases int64
	if err := db.Model(&models.ProfileRelease{}).Where("profile_id = ?", profile.ID).Count(&releases).Error; err != nil {
		t.Fatal(err)
	}
	if releases != 0 {
		t.Fatalf("published releases = %d, want 0", releases)
	}
	if _, err := os.Stat(ready); err != nil {
		t.Fatalf("fake ready was claimed: %v", err)
	}
}

func TestWatcherRejectsManuallyCreatedProcessingDirectory(t *testing.T) {
	service, db := testService(t)
	profile := models.Profile{ID: newID(), Name: "Test", Slug: "test", Loader: "fabric", GameVersion: "1.21.1", JavaVersion: 21, IsActive: true}
	if err := db.Create(&profile).Error; err != nil {
		t.Fatal(err)
	}
	generation := "fake-processing-" + newID()
	job := models.DeliveryJob{
		ID: newID(), Kind: "profile", ProfileID: &profile.ID, Generation: generation,
		Status: "waiting", Phase: "upload", Message: "upload",
	}
	if err := db.Create(&job).Error; err != nil {
		t.Fatal(err)
	}
	processing := filepath.Join(service.incomingRoot(), profile.ID, generation+".processing")
	if err := os.MkdirAll(processing, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(processing, "partial.jar"), []byte("partial"), 0644); err != nil {
		t.Fatal(err)
	}

	NewWatcher(service, nil).reconcileProfiles()
	var releases int64
	if err := db.Model(&models.ProfileRelease{}).Where("profile_id = ?", profile.ID).Count(&releases).Error; err != nil {
		t.Fatal(err)
	}
	if releases != 0 {
		t.Fatalf("published releases = %d, want 0", releases)
	}
	if _, err := os.Stat(processing); err != nil {
		t.Fatalf("manual processing directory was claimed: %v", err)
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
	broker := events.NewBroker()
	subscriptionID, eventChannel := broker.Subscribe()
	defer broker.Unsubscribe(subscriptionID)
	watcher := NewWatcher(service, broker)
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
	profileActivationSeen := false
	for {
		select {
		case event := <-eventChannel:
			if event == "profiles" {
				profileActivationSeen = true
			}
		default:
			if !profileActivationSeen {
				t.Fatal("player profile activation event was not published")
			}
			return
		}
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
	broker := events.NewBroker()
	subscriptionID, eventChannel := broker.Subscribe()
	defer broker.Unsubscribe(subscriptionID)
	NewWatcher(service, broker).reconcile()
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
	profileActivationSeen := false
	for {
		select {
		case event := <-eventChannel:
			if event == profileReleaseEvent {
				profileActivationSeen = true
			}
		default:
			if !profileActivationSeen {
				t.Fatal("recovered profile activation event was not published")
			}
			return
		}
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
	broker := events.NewBroker()
	subscriptionID, eventChannel := broker.Subscribe()
	defer broker.Unsubscribe(subscriptionID)
	NewWatcher(service, broker).reconcileLauncherJobs()
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
	activationSeen := false
	for {
		select {
		case event := <-eventChannel:
			if event == launcherReleaseEvent {
				activationSeen = true
			}
		default:
			if !activationSeen {
				t.Fatal("launcher activation event was not published after durable activation")
			}
			return
		}
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

func TestGarbageCollectRecoversInterruptedQuarantine(t *testing.T) {
	service, db := testService(t)
	profile := models.Profile{ID: newID(), Name: "Test", Slug: "test", Loader: "fabric", GameVersion: "1.21.1", JavaVersion: 21, IsActive: true}
	if err := db.Create(&profile).Error; err != nil {
		t.Fatal(err)
	}
	source := t.TempDir()
	if err := os.WriteFile(filepath.Join(source, "client.jar"), []byte("current"), 0644); err != nil {
		t.Fatal(err)
	}
	releaseID, err := service.publishProfile(context.Background(), profile.ID, source, func(_, _ int) {})
	if err != nil {
		t.Fatal(err)
	}
	manifest := service.manifestPath(releaseID)
	if err := os.Rename(manifest, manifest+gcFileSuffix); err != nil {
		t.Fatal(err)
	}

	orphanID := newID()
	orphan := filepath.Join(service.launcherRoot, "orphan"+gcLauncherMarker+orphanID)
	if err := os.MkdirAll(orphan, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(orphan, "launcher"), []byte("old"), 0644); err != nil {
		t.Fatal(err)
	}

	if _, err := service.GarbageCollect(context.Background(), 1, 24*time.Hour); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(manifest); err != nil {
		t.Fatalf("manifest with existing metadata was not restored: %v", err)
	}
	if _, err := os.Stat(manifest + gcFileSuffix); !os.IsNotExist(err) {
		t.Fatalf("manifest quarantine survived recovery: %v", err)
	}
	if _, err := os.Stat(orphan); !os.IsNotExist(err) {
		t.Fatalf("committed launcher quarantine was not finalized: %v", err)
	}
}

func TestInspectMigrationSourcesIsReadOnlyAndValidatesArtifacts(t *testing.T) {
	service, db := testService(t)
	updatePublicKey, updatePrivateKey, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	profile := models.Profile{
		ID: newID(), Name: "Test", Slug: "test", Loader: "fabric", GameVersion: "1.21.1", JavaVersion: 21, IsActive: true,
		LaunchCommandWindows: "javaw.exe -jar launcher.jar", LaunchCommandLinux: "java -jar launcher.jar",
	}
	if err := db.Create(&profile).Error; err != nil {
		t.Fatal(err)
	}
	profileSource := filepath.Join(service.profileRoot, profile.Slug, "files", "mods")
	if err := os.MkdirAll(profileSource, 0755); err != nil {
		t.Fatal(err)
	}
	profilePayload := []byte("profile-payload")
	if err := os.WriteFile(filepath.Join(profileSource, "client.jar"), profilePayload, 0644); err != nil {
		t.Fatal(err)
	}

	release := models.LauncherRelease{ID: newID(), Version: "9.9.9", IsActive: true}
	for _, platform := range []string{"linux-x64", "windows-x64"} {
		payload := []byte("launcher-" + platform)
		digest := sha256.Sum256(payload)
		fileName := "launcher"
		if platform == "windows-x64" {
			fileName = "launcher.exe"
		}
		file := models.LauncherReleaseFile{
			ID: newID(), ReleaseID: release.ID, Platform: platform, FileName: fileName,
			HashSHA256: hex.EncodeToString(digest[:]), Size: int64(len(payload)), SignatureEd25519: hex.EncodeToString(ed25519.Sign(updatePrivateKey, payload)),
		}
		release.Files = append(release.Files, file)
		path := filepath.Join(service.launcherRoot, release.Version, platform, fileName)
		if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, payload, 0755); err != nil {
			t.Fatal(err)
		}
	}
	if err := db.Create(&release).Error; err != nil {
		t.Fatal(err)
	}
	historical := models.LauncherRelease{ID: newID(), Version: "8.0.0", IsActive: true}
	for _, platform := range []string{"linux-x64", "windows-x64"} {
		payload := []byte("historical-launcher-" + platform)
		digest := sha256.Sum256(payload)
		fileName := "launcher"
		if platform == "windows-x64" {
			fileName = "launcher.exe"
		}
		historical.Files = append(historical.Files, models.LauncherReleaseFile{
			ID: newID(), ReleaseID: historical.ID, Platform: platform, FileName: fileName,
			HashSHA256: hex.EncodeToString(digest[:]), Size: int64(len(payload)),
		})
		path := filepath.Join(service.launcherRoot, historical.Version, platform, fileName)
		if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, payload, 0755); err != nil {
			t.Fatal(err)
		}
	}
	if err := db.Create(&historical).Error; err != nil {
		t.Fatal(err)
	}

	plan, err := InspectMigrationSources(context.Background(), db, service.profileRoot, service.launcherRoot, updatePublicKey)
	if err != nil {
		t.Fatal(err)
	}
	if plan.Profiles != 1 || plan.ProfileFiles != 1 || plan.LauncherReleases != 2 || plan.LauncherFiles != 4 || plan.UnsignedLegacyLauncherFiles != 2 || plan.RequiredBytes == 0 {
		t.Fatalf("migration plan = %+v", plan)
	}
	originalSignature := release.Files[0].SignatureEd25519
	if err := db.Model(&models.LauncherReleaseFile{}).Where("id = ?", release.Files[0].ID).Update("signature_ed25519", strings.Repeat("a", 128)).Error; err != nil {
		t.Fatal(err)
	}
	if _, err := InspectMigrationSources(context.Background(), db, service.profileRoot, service.launcherRoot, updatePublicKey); err == nil {
		t.Fatal("invalid launcher Ed25519 signature passed migration inspection")
	}
	if err := db.Model(&models.LauncherReleaseFile{}).Where("id = ?", release.Files[0].ID).Update("signature_ed25519", originalSignature).Error; err != nil {
		t.Fatal(err)
	}
	var deliveryRows int64
	if err := db.Model(&models.DeliveryBlob{}).Count(&deliveryRows).Error; err != nil {
		t.Fatal(err)
	}
	if deliveryRows != 0 {
		t.Fatalf("read-only inspection created %d delivery rows", deliveryRows)
	}

	corrupt := filepath.Join(service.launcherRoot, release.Version, "linux-x64", "launcher")
	if err := os.WriteFile(corrupt, []byte("corrupt-payload"), 0755); err != nil {
		t.Fatal(err)
	}
	if _, err := InspectMigrationSources(context.Background(), db, service.profileRoot, service.launcherRoot, updatePublicKey); err == nil {
		t.Fatal("corrupt launcher source passed migration inspection")
	}
	linuxPayload := []byte("launcher-linux-x64")
	if err := os.WriteFile(corrupt, linuxPayload, 0755); err != nil {
		t.Fatal(err)
	}
	if _, err := service.publishProfile(context.Background(), profile.ID, filepath.Join(service.profileRoot, profile.Slug, "files"), func(_, _ int) {}); err != nil {
		t.Fatal(err)
	}
	if err := service.ImportLauncherRelease(context.Background(), release); err != nil {
		t.Fatal(err)
	}
	if err := service.ImportLauncherRelease(context.Background(), historical); err != nil {
		t.Fatal(err)
	}
	audit, err := service.AuditMigration(context.Background(), updatePublicKey)
	if err != nil {
		t.Fatal(err)
	}
	if audit.ProfileReleases != 1 || audit.ProfileFiles != 1 || audit.LauncherReleases != 2 || audit.LauncherFiles != 4 || audit.VerifiedBytes == 0 {
		t.Fatalf("migration audit = %+v", audit)
	}
	missingPlatformTx := db.Begin()
	if missingPlatformTx.Error != nil {
		t.Fatal(missingPlatformTx.Error)
	}
	if err := missingPlatformTx.Exec("DELETE FROM launcher_delivery_artifact_chunks WHERE artifact_id IN (SELECT id FROM launcher_delivery_artifacts WHERE release_id = ? AND platform = ?)", release.ID, "windows-x64").Error; err != nil {
		t.Fatal(err)
	}
	if err := missingPlatformTx.Exec("DELETE FROM launcher_delivery_artifacts WHERE release_id = ? AND platform = ?", release.ID, "windows-x64").Error; err != nil {
		t.Fatal(err)
	}
	if err := missingPlatformTx.Exec("DELETE FROM launcher_release_files WHERE release_id = ? AND platform = ?", release.ID, "windows-x64").Error; err != nil {
		t.Fatal(err)
	}
	transactionalService := &Service{
		db: missingPlatformTx, root: service.root, profileRoot: service.profileRoot,
		launcherRoot: service.launcherRoot, signingKey: service.signingKey, stop: make(chan struct{}),
	}
	if _, err := transactionalService.AuditMigration(context.Background(), updatePublicKey); err == nil {
		t.Fatal("missing current launcher platform source and artifact passed migration audit")
	}
	if err := missingPlatformTx.Rollback().Error; err != nil {
		t.Fatal(err)
	}
	var profileChunk models.ProfileReleaseFileChunk
	if err := db.First(&profileChunk).Error; err != nil {
		t.Fatal(err)
	}
	if err := db.Delete(&profileChunk).Error; err != nil {
		t.Fatal(err)
	}
	if _, err := service.AuditMigration(context.Background(), updatePublicKey); err == nil {
		t.Fatal("missing profile chunk relation passed migration audit")
	}
	if err := db.Create(&profileChunk).Error; err != nil {
		t.Fatal(err)
	}
	var launcherChunk models.LauncherDeliveryArtifactChunk
	if err := db.First(&launcherChunk).Error; err != nil {
		t.Fatal(err)
	}
	if err := db.Delete(&launcherChunk).Error; err != nil {
		t.Fatal(err)
	}
	if _, err := service.AuditMigration(context.Background(), updatePublicKey); err == nil {
		t.Fatal("missing launcher chunk relation passed migration audit")
	}
	if err := db.Create(&launcherChunk).Error; err != nil {
		t.Fatal(err)
	}

	var blob models.DeliveryBlob
	if err := db.First(&blob).Error; err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(service.blobPath(blob.HashSHA256), bytes.Repeat([]byte("x"), int(blob.Size)), 0644); err != nil {
		t.Fatal(err)
	}
	if _, err := service.AuditMigration(context.Background(), updatePublicKey); err == nil {
		t.Fatal("corrupt CAS blob passed migration audit")
	}
}
