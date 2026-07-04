package policy

import (
	"context"
	"testing"

	"launcher-backend/internal/database"
	"launcher-backend/internal/models"

	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
)

func openTestDB(t *testing.T) *gorm.DB {
	t.Helper()
	db, err := gorm.Open(sqlite.Open("file::memory:"), &gorm.Config{})
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	if err := database.AutoMigrate(db); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	return db
}

func TestNeedsConsent(t *testing.T) {
	if !NeedsConsent(&models.User{PolicyAcceptedVersion: 0}) {
		t.Error("нулевая версия должна требовать согласие")
	}
	if NeedsConsent(&models.User{PolicyAcceptedVersion: Version}) {
		t.Error("актуальная версия не должна требовать согласие")
	}
}

func TestStatusFor(t *testing.T) {
	st := StatusFor(&models.User{PolicyAcceptedVersion: 0})
	if !st.Required || st.Version != Version {
		t.Errorf("StatusFor = %+v, want Required=true Version=%d", st, Version)
	}
}

func TestRecordConsent(t *testing.T) {
	db := openTestDB(t)
	u := models.User{ID: "11111111-1111-1111-1111-111111111111", Login: "player", ProviderUUID: "11111111-1111-1111-1111-111111111111"}
	if err := db.Create(&u).Error; err != nil {
		t.Fatalf("create user: %v", err)
	}

	if err := RecordConsent(context.Background(), db, u.ID, SourceLauncher, "1.2.3.4"); err != nil {
		t.Fatalf("RecordConsent: %v", err)
	}

	var saved models.User
	if err := db.First(&saved, "id = ?", u.ID).Error; err != nil {
		t.Fatalf("reload user: %v", err)
	}
	if saved.PolicyAcceptedVersion != Version || saved.PolicyAcceptedAt == nil {
		t.Errorf("user = ver %d at %v, want ver %d и непустое время", saved.PolicyAcceptedVersion, saved.PolicyAcceptedAt, Version)
	}

	var consents []models.PolicyConsent
	if err := db.Find(&consents).Error; err != nil {
		t.Fatalf("read consents: %v", err)
	}
	if len(consents) != 1 || consents[0].Source != SourceLauncher || consents[0].Version != Version || consents[0].IP != "1.2.3.4" {
		t.Errorf("журнал = %+v, want одна запись launcher/v%d/1.2.3.4", consents, Version)
	}
}

func TestTextNotEmpty(t *testing.T) {
	if len(Text()) < 500 {
		t.Errorf("текст политики подозрительно короткий: %d байт", len(Text()))
	}
}
