package auth

import (
	"testing"
	"time"

	"github.com/pquerna/otp"
	"github.com/pquerna/otp/totp"
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

	ok, step := validateTOTPWithStep(secret, code, now)
	if !ok {
		t.Fatal("текущий код должен валидироваться")
	}
	if want := now.Unix() / totpPeriod; step != want {
		t.Fatalf("шаг должен соответствовать текущему окну: got %d want %d", step, want)
	}

	// Тот же код на следующем шаге всё ещё валиден (skew ±1), но его шаг НЕ больше —
	// значит SignIn с TOTPLastStep=step корректно отклонит повтор (step <= last).
	if _, replayStep := validateTOTPWithStep(secret, code, now.Add(totpPeriod*time.Second)); replayStep > step {
		t.Fatalf("повтор кода не должен давать шаг больше исходного: got %d > %d", replayStep, step)
	}

	if bad, _ := validateTOTPWithStep(secret, "000000", now); bad {
		t.Fatal("неверный код не должен валидироваться")
	}
}
