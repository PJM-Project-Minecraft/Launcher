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
	if err := db.AutoMigrate(&models.Detection{}, &models.CheatSignature{}, &models.GameFile{}); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	return db
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
	svc := NewService(db, "secret", false, nil, "")
	claims := LaunchClaims{UUID: "uuid-files", Login: "Liko", Nonce: "n-files"}
	ctx := context.Background()

	// Репорт-онли (дефолт): посторонний jar пишется детектом, но игрок не кикается.
	// Хеш из сборки принимается в любом регистре.
	kick, err := svc.CheckFiles(ctx, claims, []ReportedFile{
		{Path: "mods/ok.jar", Sha256: strings.ToUpper(known)},
		{Path: "mods/x.jar", Sha256: strings.Repeat("b", 64)},
	})
	if err != nil {
		t.Fatalf("check: %v", err)
	}
	if kick {
		t.Fatal("в репорт-онли kick не ожидается")
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

	// Enforce: посторонний jar → kick. Имя другое, иначе сработает дедуп детектов.
	svc.SetEnforceUnknownMods(true)
	kick, err = svc.CheckFiles(ctx, claims, []ReportedFile{{Path: "mods/y.jar", Sha256: strings.Repeat("c", 64)}})
	if err != nil {
		t.Fatalf("check: %v", err)
	}
	if !kick {
		t.Fatal("в enforce ожидался kick за посторонний jar")
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
