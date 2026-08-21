package delivery

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"launcher-backend/internal/models"

	"gorm.io/driver/postgres"
	"gorm.io/gorm"
)

// Run with TEST_DATABASE_URL against an isolated disposable PostgreSQL. This
// specifically protects nullable UUID fields from SQLite-only assumptions.
func TestPostgresDeliveryJobUUIDNullability(t *testing.T) {
	dsn := os.Getenv("TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("TEST_DATABASE_URL is not set")
	}
	db, err := gorm.Open(postgres.Open(dsn), &gorm.Config{})
	if err != nil {
		t.Fatal(err)
	}
	if err := db.AutoMigrate(
		&models.Profile{}, &models.LauncherRelease{}, &models.LauncherReleaseFile{},
		&models.DeliveryJob{},
	); err != nil {
		t.Fatal(err)
	}
	root := t.TempDir()
	service, err := NewService(db, filepath.Join(root, "delivery"), filepath.Join(root, "profiles"), filepath.Join(root, "launcher"), "")
	if err != nil {
		t.Fatal(err)
	}
	profile := models.Profile{ID: newID(), Name: "Postgres", Slug: "postgres", Loader: "fabric", GameVersion: "1.21.1", IsActive: true}
	if err := db.Create(&profile).Error; err != nil {
		t.Fatal(err)
	}
	if _, _, err := service.CreateDraft(context.Background(), profile.ID); err != nil {
		t.Fatalf("profile job with NULL release_id: %v", err)
	}
	release := models.LauncherRelease{ID: newID(), Version: "9.9.9"}
	if err := db.Create(&release).Error; err != nil {
		t.Fatal(err)
	}
	job, err := service.QueueLauncherRelease(context.Background(), release)
	if err != nil {
		t.Fatalf("launcher job with NULL profile_id/release_id: %v", err)
	}
	if job.ProfileID != nil || job.ReleaseID != nil {
		t.Fatalf("unexpected UUID values: %+v", job)
	}
}
