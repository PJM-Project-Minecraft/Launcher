package database_test

import (
	"os"
	"testing"

	"launcher-backend/internal/config"
	"launcher-backend/internal/database"
	"launcher-backend/internal/models"
)

func TestAutoMigrateCreatesShopAndDeliverySchema(t *testing.T) {
	db, err := database.Open(config.Config{SQLitePath: t.TempDir() + "/launcher.db"})
	if err != nil {
		t.Fatal(err)
	}
	if err := database.AutoMigrate(db); err != nil {
		t.Fatal(err)
	}
	for _, model := range []any{&models.ShopItem{}, &models.Delivery{}} {
		if !db.Migrator().HasTable(model) {
			t.Fatalf("не создана таблица %T", model)
		}
	}
}

func TestAutoMigratePostgres(t *testing.T) {
	dsn := os.Getenv("TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("TEST_DATABASE_URL not set")
	}
	db, err := database.Open(config.Config{DatabaseURL: dsn})
	if err != nil {
		t.Fatal(err)
	}
	if err := database.AutoMigrate(db); err != nil {
		t.Fatal(err)
	}
	for _, model := range []any{&models.ShopItem{}, &models.Delivery{}} {
		if !db.Migrator().HasTable(model) {
			t.Fatalf("не создана postgres-таблица %T", model)
		}
	}
}
