package repo_test

import (
	"context"
	"sync"
	"testing"

	"launcher-backend/internal/auth"
	"launcher-backend/internal/database"
	"launcher-backend/internal/mcuuid"
	"launcher-backend/internal/models"
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

func TestConsumeTOTPStepIsAtomic(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	hash, _ := bcrypt.GenerateFromPassword([]byte("s3cretpass"), 10)
	uid, err := repo.RegisterNewUser(ctx, db, "AtomicTOTP", "atomic@example.com", string(hash))
	if err != nil {
		t.Fatalf("register: %v", err)
	}

	start := make(chan struct{})
	results := make(chan bool, 2)
	errs := make(chan error, 2)
	var wg sync.WaitGroup
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			ok, err := repo.ConsumeTOTPStep(ctx, db, uid, 12345)
			results <- ok
			errs <- err
		}()
	}
	close(start)
	wg.Wait()
	close(results)
	close(errs)

	for err := range errs {
		if err != nil {
			t.Fatalf("consume: %v", err)
		}
	}
	succeeded := 0
	for ok := range results {
		if ok {
			succeeded++
		}
	}
	if succeeded != 1 {
		t.Fatalf("same TOTP step consumed %d times, want exactly once", succeeded)
	}
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

// TestPwdResetLifecycle: создание заявки, дедуп pending, решение и защита от двойного клика.
func TestPwdResetLifecycle(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()

	hash, _ := bcrypt.GenerateFromPassword([]byte("pw12345678"), 10)
	uid, err := repo.RegisterNewUser(ctx, db, "ResetGuy", "reset@example.com", string(hash))
	if err != nil {
		t.Fatalf("register: %v", err)
	}

	id1, created, err := repo.CreatePwdReset(ctx, db, uid, 500)
	if err != nil || !created {
		t.Fatalf("create: id=%d created=%v err=%v", id1, created, err)
	}
	// Повторная заявка того же пользователя — дедуп на pending.
	id2, created2, err := repo.CreatePwdReset(ctx, db, uid, 500)
	if err != nil || created2 || id2 != id1 {
		t.Fatalf("dedup: id2=%d created2=%v err=%v", id2, created2, err)
	}

	pending, err := repo.ListPendingPwdResets(ctx, db)
	if err != nil || len(pending) != 1 {
		t.Fatalf("pending: %d err=%v", len(pending), err)
	}

	ok, err := repo.DecidePwdReset(ctx, db, id1, "approved", "rootadmin")
	if err != nil || !ok {
		t.Fatalf("decide: ok=%v err=%v", ok, err)
	}
	// Второй клик по той же заявке — уже решена.
	ok2, err := repo.DecidePwdReset(ctx, db, id1, "rejected", "other")
	if err != nil || ok2 {
		t.Fatalf("double decide: ok2=%v err=%v", ok2, err)
	}

	r, err := repo.GetPwdReset(ctx, db, id1)
	if err != nil || r == nil || r.Status != "approved" || r.DecidedBy != "rootadmin" {
		t.Fatalf("get: %+v err=%v", r, err)
	}

	// После решения можно подать новую заявку.
	_, created3, err := repo.CreatePwdReset(ctx, db, uid, 500)
	if err != nil || !created3 {
		t.Fatalf("re-create: created3=%v err=%v", created3, err)
	}
}

// Поиск в админке по Telegram-username: админ ищет игрока по «@ник» из чата, а в
// БД username лежит без «собаки». Логин/e-mail при этом не должны совпадать.
func TestListUsersAdminFindsByTelegramUsername(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()

	tgID := int64(555001)
	if err := db.Create(&models.User{
		ID:               "aa000000-0000-0000-0000-0000000000aa",
		Login:            "SvoCraft",
		Email:            "svo@example.com",
		TelegramID:       &tgID,
		TelegramUsername: "TgSearchProbe",
	}).Error; err != nil {
		t.Fatalf("create user: %v", err)
	}

	for _, q := range []string{"tgsearchprobe", "@TgSearchProbe", "searchpro"} {
		items, total, err := repo.ListUsersAdmin(ctx, db, q, 1)
		if err != nil {
			t.Fatalf("search %q: %v", q, err)
		}
		if total != 1 || len(items) != 1 || items[0].Login != "SvoCraft" {
			t.Fatalf("по запросу %q ожидался 1 игрок, получено total=%d items=%d", q, total, len(items))
		}
	}
	if _, total, err := repo.ListUsersAdmin(ctx, db, "нетакого", 1); err != nil || total != 0 {
		t.Fatalf("несуществующий username не должен ничего находить: total=%d err=%v", total, err)
	}
}
