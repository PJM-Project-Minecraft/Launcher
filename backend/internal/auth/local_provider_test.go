package auth

import (
	"context"
	"testing"
	"time"

	"launcher-backend/internal/database"
	"launcher-backend/internal/models"
	"launcher-backend/internal/repo"

	"github.com/pquerna/otp"
	"github.com/pquerna/otp/totp"
	"golang.org/x/crypto/bcrypt"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
)

// validateTOTPWithStep — ядро анти-replay 2FA: возвращает номер шага сработавшего
// кода, а SignIn отклоняет код с шагом ≤ последнего принятого. Проверяем, что шаг
// соответствует окну и что неверный код не проходит.
func TestValidateTOTPWithStep(t *testing.T) {
	const secret = "JBSWY3DPEHPK3PXP" // тестовый base32-секрет
	now := time.Unix(1_700_000_000, 0).UTC()

	code, err := totp.GenerateCodeCustom(secret, now, totp.ValidateOpts{
		Period: totpPeriod, Skew: 1, Digits: otp.DigitsSix, Algorithm: otp.AlgorithmSHA1,
	})
	if err != nil {
		t.Fatalf("gen: %v", err)
	}

	ok, step := ValidateTOTPWithStep(secret, code, now)
	if !ok {
		t.Fatal("текущий код должен валидироваться")
	}
	if want := now.Unix() / totpPeriod; step != want {
		t.Fatalf("шаг должен соответствовать текущему окну: got %d want %d", step, want)
	}

	// Тот же код на следующем шаге всё ещё валиден (skew ±1), но его шаг НЕ больше —
	// значит SignIn с TOTPLastStep=step корректно отклонит повтор (step <= last).
	if _, replayStep := ValidateTOTPWithStep(secret, code, now.Add(totpPeriod*time.Second)); replayStep > step {
		t.Fatalf("повтор кода не должен давать шаг больше исходного: got %d > %d", replayStep, step)
	}

	if bad, _ := ValidateTOTPWithStep(secret, "000000", now); bad {
		t.Fatal("неверный код не должен валидироваться")
	}
}

func TestCorrectPasswordBypassesAttackerInducedAccountLock(t *testing.T) {
	db, err := gorm.Open(sqlite.Open("file:auth-lockout?mode=memory&cache=shared"), &gorm.Config{})
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	if err := database.AutoMigrate(db); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	hash, _ := bcrypt.GenerateFromPassword([]byte("correct horse battery staple"), bcrypt.MinCost)
	user := models.User{ID: "11111111-1111-1111-1111-111111111111", Login: "Owner", Email: "owner@example.com", PasswordHash: string(hash), ProviderUUID: "11111111-1111-1111-1111-111111111111"}
	if err := db.Create(&user).Error; err != nil {
		t.Fatalf("create user: %v", err)
	}
	ctx := context.Background()
	for i := 0; i < repo.LoginFailMax; i++ {
		uid := user.ID
		if err := repo.InsertAuthLog(ctx, db, &uid, user.Login, "test", false, strptr("bad_password")); err != nil {
			t.Fatalf("insert auth log: %v", err)
		}
	}
	if !repo.LoginLocked(ctx, db, user.Login) {
		t.Fatal("fixture did not lock account")
	}
	if _, err := NewLocalProvider(db).SignIn(ctx, user.Login, "correct horse battery staple", ""); err != nil {
		t.Fatalf("correct owner password denied by attacker-induced lock: %v", err)
	}
}
