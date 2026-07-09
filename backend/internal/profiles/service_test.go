package profiles

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"launcher-backend/internal/models"

	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
)

func TestScanBuildsManifest(t *testing.T) {
	service := newTestService(t)
	profile, err := service.Create(context.Background(), ProfileRequest{
		Name:        "Project Test",
		Slug:        "project-test",
		Loader:      "fabric",
		GameVersion: "1.21.1",
	})
	if err != nil {
		t.Fatalf("Create() error = %v", err)
	}

	filesRoot := filepath.Join(service.storageRoot, profile.Slug, "files")
	writeTestFile(t, filepath.Join(filesRoot, "mods", "example.jar"), "mod-data")
	writeTestFile(t, filepath.Join(filesRoot, "runtime", "linux", "bin", "java"), "java")

	result, err := service.Scan(context.Background(), profile.ID)
	if err != nil {
		t.Fatalf("Scan() error = %v", err)
	}
	if result.FileCount != 2 {
		t.Fatalf("FileCount = %d, want 2", result.FileCount)
	}
	if result.TotalSize != int64(len("mod-data")+len("java")) {
		t.Fatalf("TotalSize = %d", result.TotalSize)
	}

	manifest, err := service.Manifest(context.Background(), profile.ID)
	if err != nil {
		t.Fatalf("Manifest() error = %v", err)
	}
	if manifest.FileCount != 2 {
		t.Fatalf("manifest.FileCount = %d, want 2", manifest.FileCount)
	}
	if manifest.Files[0].Path != "mods/example.jar" {
		t.Fatalf("first manifest path = %q", manifest.Files[0].Path)
	}
	if _, err := service.Download(context.Background(), profile.ID, "../escape.jar"); err == nil {
		t.Fatal("Download() accepted path traversal")
	}
}

func TestScanHandlesLargeFileCount(t *testing.T) {
	service := newTestService(t)
	profile, err := service.Create(context.Background(), ProfileRequest{
		Name:        "Project Test",
		Slug:        "project-test",
		Loader:      "fabric",
		GameVersion: "1.21.1",
	})
	if err != nil {
		t.Fatalf("Create() error = %v", err)
	}

	// Превышаем SQLITE_MAX_VARIABLE_NUMBER: 8 колонок * fileCount должно
	// уходить за лимит одиночного INSERT (32766 на современных сборках SQLite).
	const fileCount = 5000
	filesRoot := filepath.Join(service.storageRoot, profile.Slug, "files")
	for i := 0; i < fileCount; i++ {
		writeTestFile(t, filepath.Join(filesRoot, "mods", fmt.Sprintf("mod-%d.jar", i)), "data")
	}

	result, err := service.Scan(context.Background(), profile.ID)
	if err != nil {
		t.Fatalf("Scan() error = %v", err)
	}
	if result.FileCount != fileCount {
		t.Fatalf("FileCount = %d, want %d", result.FileCount, fileCount)
	}
}

func TestScanRejectsSymlink(t *testing.T) {
	service := newTestService(t)
	profile, err := service.Create(context.Background(), ProfileRequest{
		Name:        "Project Test",
		Slug:        "project-test",
		Loader:      "fabric",
		GameVersion: "1.21.1",
	})
	if err != nil {
		t.Fatalf("Create() error = %v", err)
	}

	filesRoot := filepath.Join(service.storageRoot, profile.Slug, "files")
	writeTestFile(t, filepath.Join(filesRoot, "real.jar"), "data")
	if err := os.Symlink(filepath.Join(filesRoot, "real.jar"), filepath.Join(filesRoot, "linked.jar")); err != nil {
		t.Skipf("symlink unavailable: %v", err)
	}

	_, err = service.Scan(context.Background(), profile.ID)
	if err == nil || !strings.Contains(err.Error(), "symlink") {
		t.Fatalf("Scan() error = %v, want symlink rejection", err)
	}
}

func TestScanSkipsPreservedPathsAndManifestReturnsWhitelist(t *testing.T) {
	service := newTestService(t)
	profile, err := service.Create(context.Background(), ProfileRequest{
		Name:          "Project Test",
		Slug:          "project-test",
		Loader:        "fabric",
		GameVersion:   "1.21.1",
		PreservePaths: []string{"saves/", "options.txt"},
	})
	if err != nil {
		t.Fatalf("Create() error = %v", err)
	}

	filesRoot := filepath.Join(service.storageRoot, profile.Slug, "files")
	writeTestFile(t, filepath.Join(filesRoot, "mods", "example.jar"), "mod-data")
	writeTestFile(t, filepath.Join(filesRoot, "saves", "world", "level.dat"), "save-data")
	writeTestFile(t, filepath.Join(filesRoot, "options.txt"), "options")

	result, err := service.Scan(context.Background(), profile.ID)
	if err != nil {
		t.Fatalf("Scan() error = %v", err)
	}
	if result.FileCount != 1 {
		t.Fatalf("FileCount = %d, want 1", result.FileCount)
	}

	manifest, err := service.Manifest(context.Background(), profile.ID)
	if err != nil {
		t.Fatalf("Manifest() error = %v", err)
	}
	if manifest.FileCount != 1 || manifest.Files[0].Path != "mods/example.jar" {
		t.Fatalf("manifest files = %#v, want only mods/example.jar", manifest.Files)
	}
	if !equalStrings(manifest.PreservePaths, []string{"saves/", "options.txt"}) {
		t.Fatalf("PreservePaths = %#v", manifest.PreservePaths)
	}
}

func TestDriftCleanAfterScan(t *testing.T) {
	service := newTestService(t)
	profile, err := service.Create(context.Background(), ProfileRequest{
		Name:        "Project Test",
		Slug:        "project-test",
		Loader:      "fabric",
		GameVersion: "1.21.1",
	})
	if err != nil {
		t.Fatalf("Create() error = %v", err)
	}

	filesRoot := filepath.Join(service.storageRoot, profile.Slug, "files")
	writeTestFile(t, filepath.Join(filesRoot, "mods", "example.jar"), "mod-data")
	if _, err := service.Scan(context.Background(), profile.ID); err != nil {
		t.Fatalf("Scan() error = %v", err)
	}

	drift, err := service.Drift(context.Background(), profile.ID)
	if err != nil {
		t.Fatalf("Drift() error = %v", err)
	}
	if !drift.Scanned {
		t.Fatal("Scanned = false, want true после сканирования")
	}
	if drift.Drifted {
		t.Fatalf("Drifted = true сразу после Scan, drift = %+v", drift)
	}
}

func TestDriftDetectsAddedRemovedAndResized(t *testing.T) {
	service := newTestService(t)
	profile, err := service.Create(context.Background(), ProfileRequest{
		Name:          "Project Test",
		Slug:          "project-test",
		Loader:        "fabric",
		GameVersion:   "1.21.1",
		PreservePaths: []string{"options.txt"},
	})
	if err != nil {
		t.Fatalf("Create() error = %v", err)
	}

	filesRoot := filepath.Join(service.storageRoot, profile.Slug, "files")
	writeTestFile(t, filepath.Join(filesRoot, "mods", "keep.jar"), "keep")
	writeTestFile(t, filepath.Join(filesRoot, "mods", "gone.jar"), "gone")
	writeTestFile(t, filepath.Join(filesRoot, "mods", "resized.jar"), "old")
	if _, err := service.Scan(context.Background(), profile.ID); err != nil {
		t.Fatalf("Scan() error = %v", err)
	}

	// Меняем storage мимо сканирования: +1 файл, -1 файл, у одного другой размер.
	// Preserve-путь меняться может свободно — дрифтом не считается.
	writeTestFile(t, filepath.Join(filesRoot, "mods", "added.jar"), "added")
	if err := os.Remove(filepath.Join(filesRoot, "mods", "gone.jar")); err != nil {
		t.Fatalf("Remove() error = %v", err)
	}
	writeTestFile(t, filepath.Join(filesRoot, "mods", "resized.jar"), "new-longer-content")
	writeTestFile(t, filepath.Join(filesRoot, "options.txt"), "player-options")

	drift, err := service.Drift(context.Background(), profile.ID)
	if err != nil {
		t.Fatalf("Drift() error = %v", err)
	}
	if !drift.Drifted {
		t.Fatal("Drifted = false, want true")
	}
	if drift.Added != 1 || drift.Removed != 1 || drift.Changed != 1 {
		t.Fatalf("drift = %+v, want Added=1 Removed=1 Changed=1", drift)
	}
}

func TestDriftDetectsSameSizeMtimeBump(t *testing.T) {
	service := newTestService(t)
	profile, err := service.Create(context.Background(), ProfileRequest{
		Name:        "Project Test",
		Slug:        "project-test",
		Loader:      "fabric",
		GameVersion: "1.21.1",
	})
	if err != nil {
		t.Fatalf("Create() error = %v", err)
	}

	filesRoot := filepath.Join(service.storageRoot, profile.Slug, "files")
	target := filepath.Join(filesRoot, "mods", "swapped.jar")
	writeTestFile(t, target, "AAAA")
	if _, err := service.Scan(context.Background(), profile.ID); err != nil {
		t.Fatalf("Scan() error = %v", err)
	}

	// Подмена содержимого тем же размером: ловится только по mtime новее скана.
	writeTestFile(t, target, "BBBB")
	future := time.Now().Add(time.Hour)
	if err := os.Chtimes(target, future, future); err != nil {
		t.Fatalf("Chtimes() error = %v", err)
	}

	drift, err := service.Drift(context.Background(), profile.ID)
	if err != nil {
		t.Fatalf("Drift() error = %v", err)
	}
	if !drift.Drifted || drift.Changed != 1 {
		t.Fatalf("drift = %+v, want Drifted=true Changed=1", drift)
	}
}

func TestDriftBeforeAnyScan(t *testing.T) {
	service := newTestService(t)
	profile, err := service.Create(context.Background(), ProfileRequest{
		Name:        "Project Test",
		Slug:        "project-test",
		Loader:      "fabric",
		GameVersion: "1.21.1",
	})
	if err != nil {
		t.Fatalf("Create() error = %v", err)
	}

	// Пустой несканированный профиль — дрифта нет (нечего сканировать).
	drift, err := service.Drift(context.Background(), profile.ID)
	if err != nil {
		t.Fatalf("Drift() error = %v", err)
	}
	if drift.Scanned || drift.Drifted {
		t.Fatalf("drift = %+v, want Scanned=false Drifted=false для пустого профиля", drift)
	}

	// Файлы появились, но манифест ни разу не собирался — это дрифт.
	filesRoot := filepath.Join(service.storageRoot, profile.Slug, "files")
	writeTestFile(t, filepath.Join(filesRoot, "mods", "example.jar"), "mod-data")

	drift, err = service.Drift(context.Background(), profile.ID)
	if err != nil {
		t.Fatalf("Drift() error = %v", err)
	}
	if drift.Scanned || !drift.Drifted || drift.Added != 1 {
		t.Fatalf("drift = %+v, want Scanned=false Drifted=true Added=1", drift)
	}
}

func TestPreservePathsRejectUnsafeAndReservedPaths(t *testing.T) {
	service := newTestService(t)
	values := []string{
		"",
		"../escape",
		"/absolute",
		"mods/",
		"libraries/example.jar",
		"versions/1.21.1/1.21.1.jar",
		"assets/indexes/1.json",
		"runtime/linux/bin/java",
		"C:/Users/player/options.txt",
	}

	for _, value := range values {
		_, err := service.Create(context.Background(), ProfileRequest{
			Name:          "Project Test " + strings.ReplaceAll(value, "/", "-"),
			Slug:          "project-test",
			Loader:        "fabric",
			GameVersion:   "1.21.1",
			PreservePaths: []string{value},
		})
		if err == nil {
			t.Fatalf("Create() accepted preserve path %q", value)
		}
	}
}

func TestClientPreparedAcceptsInstalledNeoForgeVersionJSON(t *testing.T) {
	service := NewService(nil, t.TempDir())
	profile := models.Profile{
		Name:          "Project Test",
		Slug:          "project-test",
		Loader:        "neoforge",
		GameVersion:   "1.21.1",
		LoaderVersion: "21.1.233",
	}
	filesRoot := filepath.Join(service.storageRoot, profile.Slug, "files")
	writeTestFile(t, filepath.Join(filesRoot, "versions", "1.21.1", "1.21.1.json"), "{}")
	writeTestFile(t, filepath.Join(filesRoot, "versions", "1.21.1", "1.21.1.jar"), "client")
	writeTestFile(
		t,
		filepath.Join(filesRoot, "versions", "neoforge-21.1.233", "neoforge-21.1.233.json"),
		`{"id":"neoforge-21.1.233","inheritsFrom":"1.21.1"}`,
	)

	if !service.clientPrepared(profile) {
		t.Fatal("clientPrepared() = false, want true for installed NeoForge version JSON")
	}
}

func TestUpdatePreservesGeneratedLaunchCommandForModdedLoader(t *testing.T) {
	service := newTestService(t)
	created, err := service.Create(context.Background(), ProfileRequest{
		Name:        "Project Test",
		Slug:        "project-test",
		Loader:      "neoforge",
		GameVersion: "1.21.1",
	})
	if err != nil {
		t.Fatalf("Create() error = %v", err)
	}

	// Имитируем результат «Подготовить клиент»: сгенерированная команда в БД.
	const generated = `"{java}" {jvm_args} -cp libraries/net/neoforged/loader.jar net.neoforged.Main`
	if err := service.db.Model(&models.Profile{}).Where("id = ?", created.ID).
		Updates(map[string]any{
			"launch_command_windows": generated,
			"launch_command_linux":   generated,
			"launch_command_mac_os":  generated,
		}).Error; err != nil {
		t.Fatalf("seed launch command: %v", err)
	}

	// Дашборд сохраняет профиль с ванильным плейсхолдером команды — он не должен затереть сгенерированную.
	if _, err := service.Update(context.Background(), created.ID, ProfileRequest{
		Name:                 "Project Test",
		Slug:                 "project-test",
		Loader:               "neoforge",
		GameVersion:          "1.21.1",
		LaunchCommandWindows: "{java} {jvm_args} -jar client.jar",
		LaunchCommandLinux:   "{java} {jvm_args} -jar client.jar",
		LaunchCommandMacOS:   "{java} {jvm_args} -jar client.jar",
	}); err != nil {
		t.Fatalf("Update() error = %v", err)
	}

	var profile models.Profile
	if err := service.db.First(&profile, "id = ?", created.ID).Error; err != nil {
		t.Fatalf("reload profile: %v", err)
	}
	if profile.LaunchCommandLinux != generated {
		t.Fatalf("LaunchCommandLinux = %q, want generated command preserved", profile.LaunchCommandLinux)
	}
	if profile.LaunchCommandWindows != generated || profile.LaunchCommandMacOS != generated {
		t.Fatalf("modded launch commands were overwritten: win=%q mac=%q", profile.LaunchCommandWindows, profile.LaunchCommandMacOS)
	}
}

func TestUpdateHonorsLaunchCommandForVanillaLoader(t *testing.T) {
	service := newTestService(t)
	created, err := service.Create(context.Background(), ProfileRequest{
		Name:        "Vanilla Test",
		Slug:        "vanilla-test",
		Loader:      "vanilla",
		GameVersion: "1.21.1",
	})
	if err != nil {
		t.Fatalf("Create() error = %v", err)
	}

	const custom = `{java} {jvm_args} -jar client.jar --custom`
	if _, err := service.Update(context.Background(), created.ID, ProfileRequest{
		Name:               "Vanilla Test",
		Slug:               "vanilla-test",
		Loader:             "vanilla",
		GameVersion:        "1.21.1",
		LaunchCommandLinux: custom,
	}); err != nil {
		t.Fatalf("Update() error = %v", err)
	}

	var profile models.Profile
	if err := service.db.First(&profile, "id = ?", created.ID).Error; err != nil {
		t.Fatalf("reload profile: %v", err)
	}
	if profile.LaunchCommandLinux != custom {
		t.Fatalf("LaunchCommandLinux = %q, want custom command honored for vanilla", profile.LaunchCommandLinux)
	}
}

func equalStrings(left, right []string) bool {
	if len(left) != len(right) {
		return false
	}
	for index := range left {
		if left[index] != right[index] {
			return false
		}
	}
	return true
}

func newTestService(t *testing.T) Service {
	t.Helper()

	dsn := "file:" + strings.ReplaceAll(t.Name(), "/", "_") + "?mode=memory&cache=shared"
	db, err := gorm.Open(sqlite.Open(dsn), &gorm.Config{})
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	if err := db.AutoMigrate(&models.Profile{}, &models.GameFile{}); err != nil {
		t.Fatalf("AutoMigrate: %v", err)
	}
	return NewService(db, t.TempDir())
}

func writeTestFile(t *testing.T, path string, data string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		t.Fatalf("MkdirAll(%q): %v", path, err)
	}
	if err := os.WriteFile(path, []byte(data), 0644); err != nil {
		t.Fatalf("WriteFile(%q): %v", path, err)
	}
}
