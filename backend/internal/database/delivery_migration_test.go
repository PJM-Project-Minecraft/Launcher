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

func TestPostgresQuerySurvivesResultTypeChange(t *testing.T) {
	dsn := os.Getenv("TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("TEST_DATABASE_URL not set")
	}
	db, err := database.Open(config.Config{DatabaseURL: dsn})
	if err != nil {
		t.Fatal(err)
	}
	if err := db.Exec("DROP TABLE IF EXISTS delivery_plan_regression").Error; err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Exec("DROP TABLE IF EXISTS delivery_plan_regression").Error })
	if err := db.Exec("CREATE TABLE delivery_plan_regression (value text NOT NULL)").Error; err != nil {
		t.Fatal(err)
	}
	if err := db.Exec("INSERT INTO delivery_plan_regression(value) VALUES ('before')").Error; err != nil {
		t.Fatal(err)
	}
	var before []struct{ Value string }
	if err := db.Raw("SELECT * FROM delivery_plan_regression").Scan(&before).Error; err != nil {
		t.Fatal(err)
	}
	if err := db.Exec("ALTER TABLE delivery_plan_regression ALTER COLUMN value TYPE varchar(64)").Error; err != nil {
		t.Fatal(err)
	}
	var after []struct{ Value string }
	if err := db.Raw("SELECT * FROM delivery_plan_regression").Scan(&after).Error; err != nil {
		t.Fatalf("post-migration query reused an incompatible cached plan: %v", err)
	}
}
