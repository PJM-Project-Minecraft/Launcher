package auth

import (
	"context"
	"crypto/subtle"
	"net/http"
	"strings"
	"time"

	"launcher-backend/internal/models"
	"launcher-backend/internal/repo"

	"github.com/pquerna/otp"
	"github.com/pquerna/otp/totp"
	"golang.org/x/crypto/bcrypt"
	"gorm.io/gorm"
)

const (
	// loginFailWindow / loginFailMax — per-account throттлинг перебора, НЕЗАВИСИМЫЙ от IP:
	// per-IP лимитер обходится спреем с ботнета, а этот считает неудачи по логину.
	// ponytail: жёсткий лок на окно (при ≥Max корректный пароль тоже даёт 429). Порог
	// высокий, чтобы обычные опечатки не триггерили; griefing-лок максимум на окно и
	// авто-истекает, т.к. на 429 мы НЕ пишем новую неудачу (иначе атакующий держал бы лок вечно).
	loginFailWindow = 15 * time.Minute
	loginFailMax    = 30
	// totpPeriod — период TOTP (сек), как в totp.Validate по умолчанию.
	totpPeriod = 30
)

// dummyBcryptHash — валидный bcrypt-хеш для выравнивания времени ответа на путях
// «логин не найден» / «пароль не задан»: без него эти ветки отвечают за микросекунды,
// а реальная проверка пароля — ~80мс, и по таймингу различались существующие аккаунты.
var dummyBcryptHash = func() []byte {
	h, _ := bcrypt.GenerateFromPassword([]byte("timing-equalizer-not-a-secret"), bcrypt.DefaultCost)
	return h
}()

// validateTOTPWithStep проверяет код TOTP (period 30, skew ±1, 6 цифр, SHA1 — как
// totp.Validate) и возвращает номер сработавшего шага (unix/30). Шаг нужен для анти-
// replay: код с шагом ≤ последнего принятого отклоняется. Сравнение — constant-time.
func validateTOTPWithStep(secret, code string, now time.Time) (bool, int64) {
	curStep := now.Unix() / totpPeriod
	opts := totp.ValidateOpts{Period: totpPeriod, Skew: 1, Digits: otp.DigitsSix, Algorithm: otp.AlgorithmSHA1}
	for delta := int64(-1); delta <= 1; delta++ {
		step := curStep + delta
		want, err := totp.GenerateCodeCustom(secret, time.Unix(step*totpPeriod, 0), opts)
		if err != nil {
			continue
		}
		if subtle.ConstantTimeCompare([]byte(code), []byte(want)) == 1 {
			return true, step
		}
	}
	return false, 0
}

// LocalProvider проверяет логин/пароль/2FA прямо в общей БД (bcrypt + TOTP).
// Логика повторяет прежний GML-эндпоинт Telegram-бота: поиск по нику/логину/почте,
// проверка блокировок, bcrypt, при включённой 2FA — TOTP-код.
type LocalProvider struct {
	db *gorm.DB
}

func NewLocalProvider(db *gorm.DB) LocalProvider {
	return LocalProvider{db: db}
}

func (p LocalProvider) SignIn(ctx context.Context, login, password, totpCode string) (ProviderSignInResponse, error) {
	login = strings.TrimSpace(login)
	badCreds := ProviderError{StatusCode: http.StatusUnauthorized, Message: "Неверный логин или пароль"}

	user, err := repo.FindUserLogin(ctx, p.db, login)
	if err != nil {
		return ProviderSignInResponse{}, err
	}
	if user == nil {
		// Выравниваем тайминг с реальной проверкой пароля (анти-энумерация аккаунтов).
		_ = bcrypt.CompareHashAndPassword(dummyBcryptHash, []byte(password))
		_ = repo.InsertAuthLog(ctx, p.db, nil, login, "launcher", false, strptr("not_found"))
		return ProviderSignInResponse{}, badCreds
	}

	uid := user.ID
	if user.IsBanned || user.IsHwidBanned {
		_ = repo.InsertAuthLog(ctx, p.db, &uid, user.Login, "launcher", false, strptr("banned"))
		return ProviderSignInResponse{}, ProviderError{StatusCode: http.StatusForbidden, Message: "Аккаунт заблокирован"}
	}

	// Per-account анти-брутфорс: при слишком многих недавних неудачах отклоняем ДО bcrypt.
	// На этой ветке auth_log НЕ пишем — иначе счётчик не истечёт и лок стал бы вечным.
	if fails, cErr := repo.CountRecentFailedLogins(ctx, p.db, user.Login, time.Now().UTC().Add(-loginFailWindow)); cErr == nil && fails >= loginFailMax {
		return ProviderSignInResponse{}, ProviderError{
			StatusCode: http.StatusTooManyRequests, Message: "Слишком много неудачных попыток входа. Попробуйте позже.",
		}
	}

	if user.PasswordHash == "" {
		_ = bcrypt.CompareHashAndPassword(dummyBcryptHash, []byte(password)) // тайминг как у реальной проверки
		_ = repo.InsertAuthLog(ctx, p.db, &uid, user.Login, "launcher", false, strptr("bad_password"))
		return ProviderSignInResponse{}, badCreds
	}
	if bcrypt.CompareHashAndPassword([]byte(user.PasswordHash), []byte(password)) != nil {
		_ = repo.InsertAuthLog(ctx, p.db, &uid, user.Login, "launcher", false, strptr("bad_password"))
		return ProviderSignInResponse{}, badCreds
	}

	if user.TOTPEnabled && user.TOTPSecret != "" {
		code := strings.ReplaceAll(strings.TrimSpace(totpCode), " ", "")
		if code == "" {
			_ = repo.InsertAuthLog(ctx, p.db, &uid, user.Login, "launcher", false, strptr("totp_required"))
			return ProviderSignInResponse{}, ProviderError{
				StatusCode: http.StatusUnauthorized, Message: twoFactorMessage, RequiresTwoFactor: true,
			}
		}
		ok, step := validateTOTPWithStep(user.TOTPSecret, code, time.Now().UTC())
		// step <= TOTPLastStep — код уже использовался в этом окне (replay): отклоняем
		// тем же сообщением, что и неверный код (не выдаём, что код был правильным).
		if !ok || step <= user.TOTPLastStep {
			_ = repo.InsertAuthLog(ctx, p.db, &uid, user.Login, "launcher", false, strptr("invalid_totp"))
			return ProviderSignInResponse{}, ProviderError{
				StatusCode: http.StatusUnauthorized, Message: "Неверный код двухфакторной аутентификации", RequiresTwoFactor: true,
			}
		}
		_ = repo.SetTOTPLastStep(ctx, p.db, user.ID, step)
	}

	_ = repo.InsertAuthLog(ctx, p.db, &uid, user.Login, "launcher", true, strptr("OK"))
	return ProviderSignInResponse{
		Login:    user.Login,
		UserUUID: user.ProviderUUID,
		IsSlim:   user.IsSlim,
		Message:  "Успешная авторизация",
	}, nil
}

// MarkLogin фиксирует ip/hwid после успешного входа (для GML-эндпоинта лаунчера).
func (p LocalProvider) MarkLogin(ctx context.Context, providerUUID, ip, hardwareID string) {
	var u models.User
	if err := p.db.WithContext(ctx).Where("provider_uuid = ?", providerUUID).First(&u).Error; err != nil {
		return
	}
	_ = repo.UpdateUserAfterGMLLogin(ctx, p.db, u.ID, ip, hardwareID)
}

func strptr(s string) *string { return &s }
