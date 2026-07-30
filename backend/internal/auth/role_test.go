package auth

import (
	"context"
	"testing"
	"time"

	"launcher-backend/internal/models"

	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

func newRoleTestDB(t *testing.T) *gorm.DB {
	t.Helper()
	db, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{Logger: logger.Discard})
	if err != nil {
		t.Fatalf("db: %v", err)
	}
	if err := db.AutoMigrate(&models.User{}); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	return db
}

// Сравнение с ADMIN_LOGINS обязано быть регистрозависимым: логины в БД уникальны с
// учётом регистра, поэтому регистронезависимый матч давал эскалацию — игрок
// регистрировал «Likonchik» при админе «likonchik» и получал роль admin.
func TestRoleForLoginIsCaseSensitive(t *testing.T) {
	db := newRoleTestDB(t)
	svc := NewService(db, nil, "secret", []string{"likonchik"}, "production", time.Hour)

	role, err := svc.roleForLogin(context.Background(), "Likonchik", "uuid-attacker")
	if err != nil {
		t.Fatalf("roleForLogin: %v", err)
	}
	if role != "user" {
		t.Fatalf("ник-двойник в другом регистре не должен давать admin, получено %q", role)
	}

	role, err = svc.roleForLogin(context.Background(), "likonchik", "uuid-admin")
	if err != nil {
		t.Fatalf("roleForLogin: %v", err)
	}
	if role != "admin" {
		t.Fatalf("точное совпадение с ADMIN_LOGINS обязано давать admin, получено %q", role)
	}
}
