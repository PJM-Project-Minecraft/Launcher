package repo_test

import (
	"context"
	"testing"

	"launcher-backend/internal/auth"
	"launcher-backend/internal/database"
	"launcher-backend/internal/mcuuid"
	"launcher-backend/internal/repo"

	"golang.org/x/crypto/bcrypt"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
)

func newTestDB(t *testing.T) *gorm.DB {
	t.Helper()
	db, err := gorm.Open(sqlite.Open("file::memory:?cache=shared"), &gorm.Config{})
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	if err := database.AutoMigrate(db); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	return db
}

// TestRegisterAndLocalLogin проверяет сквозной путь: регистрация (как в боте) →
// локальная аутентификация (как из лаунчера через LocalProvider), включая совпадение UUID.
func TestRegisterAndLocalLogin(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()

	hash, _ := bcrypt.GenerateFromPassword([]byte("s3cretpass"), 10)
	uid, err := repo.RegisterNewUser(ctx, db, "Likonchik", "lik@example.com", string(hash))
	if err != nil {
		t.Fatalf("register: %v", err)
	}

	// UUID должен совпадать с offline-UUID Minecraft.
	wantUUID, _ := mcuuid.OfflinePlayerUUIDString("Likonchik")
	if uid != wantUUID {
		t.Fatalf("uuid mismatch: got %s want %s", uid, wantUUID)
	}

	// Дубликат логина → ErrDuplicate.
	if _, err := repo.RegisterNewUser(ctx, db, "Likonchik", "other@example.com", string(hash)); err != repo.ErrDuplicate {
		t.Fatalf("expected ErrDuplicate, got %v", err)
	}

	// Поиск по логину (== игровой ник при регистрации) и по почте.
	if u, err := repo.FindUserLogin(ctx, db, "Likonchik"); err != nil || u == nil {
		t.Fatalf("find by login: %v %v", u, err)
	}

	// Локальная аутентификация через провайдер лаунчера.
	provider := auth.NewLocalProvider(db)
	res, err := provider.SignIn(ctx, "lik@example.com", "s3cretpass", "")
	if err != nil {
		t.Fatalf("local sign-in: %v", err)
	}
	if res.UserUUID != wantUUID || res.Login != "Likonchik" {
		t.Fatalf("unexpected sign-in result: %+v", res)
	}

	// Неверный пароль → ошибка.
	if _, err := provider.SignIn(ctx, "Likonchik", "wrong", ""); err == nil {
		t.Fatalf("expected error for wrong password")
	}
}

// TestDialoguePersistence проверяет сохранение/чтение FSM-состояния (OnConflict по chat_id).
func TestDialoguePersistence(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()

	login := "tester"
	p := repo.EmptyPayload()
	p.Login = &login
	if err := repo.SaveDialogue(ctx, db, 42, repo.FlowLinkPassword, &p); err != nil {
		t.Fatalf("save: %v", err)
	}
	// Перезапись того же chat_id (OnConflict).
	p2 := repo.EmptyPayload()
	if err := repo.SaveDialogue(ctx, db, 42, repo.FlowRegOtp, &p2); err != nil {
		t.Fatalf("save2: %v", err)
	}
	st, _, err := repo.ReadDialogue(ctx, db, 42)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if st != repo.FlowRegOtp {
		t.Fatalf("state mismatch: got %v", st)
	}
}

// TestMenuMessagePersistence: upsert id меню-сообщения по chat_id и чтение;
// отсутствие записи — 0 без ошибки.
func TestMenuMessagePersistence(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()

	got, err := repo.ReadMenuMessage(ctx, db, 77)
	if err != nil || got != 0 {
		t.Fatalf("пустое чтение: got=%d err=%v", got, err)
	}
	if err := repo.SaveMenuMessage(ctx, db, 77, 1001); err != nil {
		t.Fatalf("save: %v", err)
	}
	if err := repo.SaveMenuMessage(ctx, db, 77, 1002); err != nil {
		t.Fatalf("save-2 (upsert): %v", err)
	}
	got, err = repo.ReadMenuMessage(ctx, db, 77)
	if err != nil || got != 1002 {
		t.Fatalf("после upsert: got=%d err=%v", got, err)
	}
}
