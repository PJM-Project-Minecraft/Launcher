package anticheat

import (
	"context"
	"strings"
	"testing"

	"launcher-backend/internal/models"

	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
)

func newFilesTestDB(t *testing.T, dsn string) *gorm.DB {
	t.Helper()
	db, err := gorm.Open(sqlite.Open(dsn), &gorm.Config{})
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	if err := db.AutoMigrate(
		&models.Detection{}, &models.CheatSignature{}, &models.GameFile{},
		&models.ProfileRelease{}, &models.ProfileReleaseFile{},
		&models.AccountBan{}, &models.HwidBan{}, &models.Hwid{},
	); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	return db
}

func TestCheckFilesAcceptsNewHashFromActiveDeliveryRelease(t *testing.T) {
	db := newFilesTestDB(t, "file:acfiles-delivery?mode=memory&cache=shared")
	if err := db.Create(&models.GameFile{
		ID: "legacy", ProfileID: "p1", Name: "old.jar", Path: "mods/old.jar",
		URL: "u", HashSHA256: strings.Repeat("a", 64), Size: 1, FileType: "mod",
	}).Error; err != nil {
		t.Fatalf("seed legacy: %v", err)
	}
	release := models.ProfileRelease{
		ID: "release-active", ProfileID: "p1", Sequence: 2,
		ManifestSHA256: strings.Repeat("b", 64), IsActive: true,
	}
	if err := db.Create(&release).Error; err != nil {
		t.Fatalf("seed release: %v", err)
	}
	newHash := strings.Repeat("c", 64)
	if err := db.Create(&models.ProfileReleaseFile{
		ID: "release-file", ReleaseID: release.ID, Path: "mods/pjmapi-0.1.0.jar",
		HashSHA256: newHash, Size: 1,
	}).Error; err != nil {
		t.Fatalf("seed release file: %v", err)
	}

	svc := NewService(db, "secret", true, nil, "")
	kick, err := svc.CheckFiles(context.Background(), LaunchClaims{
		UUID: "uuid-delivery", Login: "Liko", Nonce: "n-delivery",
	}, []ReportedFile{{Path: "mods/pjmapi-0.1.0.jar", Sha256: newHash}})
	if err != nil {
		t.Fatalf("check: %v", err)
	}
	if kick {
		t.Fatal("active Delivery v2 mod was treated as unknown")
	}
	var detections int64
	if err := db.Model(&models.Detection{}).Where("user_uuid = ?", "uuid-delivery").Count(&detections).Error; err != nil {
		t.Fatalf("count detections: %v", err)
	}
	if detections != 0 {
		t.Fatalf("delivery mod created %d false detections", detections)
	}
}

func TestCheckFilesRefreshesCachedHashesAfterDeliveryPublish(t *testing.T) {
	db := newFilesTestDB(t, "file:acfiles-delivery-cache?mode=memory&cache=shared")
	legacyHash := strings.Repeat("a", 64)
	if err := db.Create(&models.GameFile{
		ID: "legacy-cache", ProfileID: "p1", Name: "old.jar", Path: "mods/old.jar",
		URL: "u", HashSHA256: legacyHash, Size: 1, FileType: "mod",
	}).Error; err != nil {
		t.Fatalf("seed legacy: %v", err)
	}
	svc := NewService(db, "secret", true, nil, "")
	claims := LaunchClaims{UUID: "uuid-cache", Login: "Liko", Nonce: "n-cache"}
	if kick, err := svc.CheckFiles(context.Background(), claims, []ReportedFile{{Path: "mods/old.jar", Sha256: legacyHash}}); err != nil || kick {
		t.Fatalf("prime cache: kick=%v err=%v", kick, err)
	}

	release := models.ProfileRelease{
		ID: "release-cache", ProfileID: "p1", Sequence: 2,
		ManifestSHA256: strings.Repeat("b", 64), IsActive: true,
	}
	if err := db.Create(&release).Error; err != nil {
		t.Fatalf("seed release: %v", err)
	}
	newHash := strings.Repeat("c", 64)
	if err := db.Create(&models.ProfileReleaseFile{
		ID: "release-cache-file", ReleaseID: release.ID, Path: "mods/pjmapi-0.1.0.jar",
		HashSHA256: newHash, Size: 1,
	}).Error; err != nil {
		t.Fatalf("seed release file: %v", err)
	}

	kick, err := svc.CheckFiles(context.Background(), claims, []ReportedFile{{Path: "mods/pjmapi-0.1.0.jar", Sha256: newHash}})
	if err != nil {
		t.Fatalf("check after publish: %v", err)
	}
	if kick {
		t.Fatal("freshly published Delivery v2 hash was hidden by the allowed-hash cache")
	}
}

func TestCheckFilesKeepsCachedEnforcementWhenRefreshFails(t *testing.T) {
	db := newFilesTestDB(t, "file:acfiles-refresh-failure?mode=memory&cache=shared")
	release := models.ProfileRelease{
		ID: "release-refresh-failure", ProfileID: "p1", Sequence: 1,
		ManifestSHA256: strings.Repeat("a", 64), IsActive: true,
	}
	if err := db.Create(&release).Error; err != nil {
		t.Fatalf("seed release: %v", err)
	}
	knownHash := strings.Repeat("b", 64)
	if err := db.Create(&models.ProfileReleaseFile{
		ID: "release-refresh-known", ReleaseID: release.ID, Path: "mods/known.jar",
		HashSHA256: knownHash, Size: 1,
	}).Error; err != nil {
		t.Fatalf("seed release file: %v", err)
	}
	svc := NewService(db, "secret", true, nil, "")
	claims := LaunchClaims{UUID: "uuid-refresh-failure", Login: "Liko", Nonce: "n-refresh-failure"}
	if kick, err := svc.CheckFiles(context.Background(), claims, []ReportedFile{{Path: "mods/known.jar", Sha256: knownHash}}); err != nil || kick {
		t.Fatalf("prime cache: kick=%v err=%v", kick, err)
	}
	if err := db.Migrator().DropTable(&models.ProfileReleaseFile{}); err != nil {
		t.Fatalf("break refresh query: %v", err)
	}

	kick, err := svc.CheckFiles(context.Background(), claims, []ReportedFile{{
		Path: "mods/unknown.jar", Sha256: strings.Repeat("c", 64),
	}})
	if err != nil {
		t.Fatalf("cached enforcement returned error: %v", err)
	}
	if !kick {
		t.Fatal("refresh failure made a cached unknown mod fail open")
	}
}

func TestCheckFilesDetectsUnknownJar(t *testing.T) {
	db := newFilesTestDB(t, "file:acfiles?mode=memory&cache=shared")
	known := strings.Repeat("a", 64)
	if err := db.Create(&models.GameFile{
		ID: "f1", ProfileID: "p1", Name: "ok.jar", Path: "mods/ok.jar",
		URL: "u", HashSHA256: known, Size: 1, FileType: "mod",
	}).Error; err != nil {
		t.Fatalf("seed: %v", err)
	}
	svc := NewService(db, "secret", true, nil, "")
	claims := LaunchClaims{UUID: "uuid-files", Login: "Liko", Nonce: "n-files"}
	ctx := context.Background()

	// Посторонний jar закрывает игру. Хеш из сборки принимается в любом регистре.
	// Даже при autoBan=true несовпадение сборки НЕ банит аккаунт или HWID.
	kick, err := svc.CheckFiles(ctx, claims, []ReportedFile{
		{Path: "mods/ok.jar", Sha256: strings.ToUpper(known)},
		{Path: "mods/x.jar", Sha256: strings.Repeat("b", 64)},
	})
	if err != nil {
		t.Fatalf("check: %v", err)
	}
	if !kick {
		t.Fatal("посторонний jar должен закрыть игру")
	}
	var detections []models.Detection
	if err := db.Where("user_uuid = ?", claims.UUID).Find(&detections).Error; err != nil {
		t.Fatalf("select: %v", err)
	}
	if len(detections) != 1 || detections[0].Signature != "x.jar" || detections[0].Type != unknownModType {
		t.Fatalf("ожидался один детект по x.jar: %+v", detections)
	}
	if detections[0].Confidence != "hard" || detections[0].Severity < 7 {
		t.Fatalf("детект должен быть hard и кикабельным: %+v", detections[0])
	}
	var accountBans, hwidBans int64
	db.Model(&models.AccountBan{}).Where("user_uuid = ?", claims.UUID).Count(&accountBans)
	db.Model(&models.HwidBan{}).Count(&hwidBans)
	if accountBans != 0 || hwidBans != 0 {
		t.Fatalf("несовпадение сборки не должно банить: accounts=%d hwids=%d", accountBans, hwidBans)
	}

	// Имя другое, иначе сработает дедуп детектов.
	kick, err = svc.CheckFiles(ctx, claims, []ReportedFile{{Path: "mods/y.jar", Sha256: strings.Repeat("c", 64)}})
	if err != nil {
		t.Fatalf("check: %v", err)
	}
	if !kick {
		t.Fatal("ожидалось закрытие игры за посторонний jar")
	}

	// Jar загрузился и исчез с диска (self-delete): хеша нет, но это тоже детект.
	kick, err = svc.CheckFiles(ctx, claims, []ReportedFile{{Path: "mods/gone.jar", Missing: true}})
	if err != nil {
		t.Fatalf("check: %v", err)
	}
	if !kick {
		t.Fatal("исчезнувший jar должен кикать в enforce")
	}
}

func TestCheckFilesFailsOpenWithoutKnownHashes(t *testing.T) {
	// Ни одной сборки в БД — сверять не с чем. Кикать всех подряд нельзя.
	svc := NewService(newFilesTestDB(t, "file:acfiles-empty?mode=memory&cache=shared"), "secret", false, nil, "")
	svc.SetEnforceUnknownMods(true)

	kick, err := svc.CheckFiles(context.Background(),
		LaunchClaims{UUID: "uuid-empty", Login: "Bob", Nonce: "n-empty"},
		[]ReportedFile{{Path: "mods/x.jar", Sha256: strings.Repeat("d", 64)}})
	if err != nil {
		t.Fatalf("check: %v", err)
	}
	if kick {
		t.Fatal("без списка хешей проверка обязана быть fail-open")
	}
}
