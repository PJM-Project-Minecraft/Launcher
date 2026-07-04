package auth

import (
	"context"
	"errors"
	"strings"
	"time"

	"launcher-backend/internal/models"
	"launcher-backend/internal/policy"

	"github.com/golang-jwt/jwt/v5"
	"github.com/google/uuid"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

type Service struct {
	db                 *gorm.DB
	provider           Provider
	jwtSecret          []byte
	adminLogins        map[string]struct{}
	autoAdminWhenEmpty bool
	tokenTTL           time.Duration
}

type LoginResult struct {
	Token     string        `json:"token"`
	ExpiresAt time.Time     `json:"expiresAt"`
	User      models.User   `json:"user"`
	Message   string        `json:"message"`
	Policy    policy.Status `json:"policy"`
}

func NewService(db *gorm.DB, provider Provider, jwtSecret string, adminLogins []string, appEnv string, tokenTTL time.Duration) Service {
	normalizedAdminLogins := normalizeAdminLogins(adminLogins)
	if tokenTTL <= 0 {
		tokenTTL = 7 * 24 * time.Hour
	}
	return Service{
		db:                 db,
		provider:           provider,
		jwtSecret:          []byte(jwtSecret),
		adminLogins:        normalizedAdminLogins,
		autoAdminWhenEmpty: len(normalizedAdminLogins) == 0 && normalizeLogin(appEnv) == "development",
		tokenTTL:           tokenTTL,
	}
}

func (s Service) Login(ctx context.Context, login, password, totp string) (LoginResult, error) {
	providerUser, err := s.provider.SignIn(ctx, login, password, totp)
	if err != nil {
		return LoginResult{}, err
	}

	now := time.Now().UTC()
	providerUUID := providerUser.UserUUID
	if providerUUID == "" {
		providerUUID = uuid.NewString()
	}

	role, err := s.roleForLogin(ctx, providerUser.Login, providerUUID)
	if err != nil {
		return LoginResult{}, err
	}

	user := models.User{
		ID:           uuid.NewString(),
		Login:        providerUser.Login,
		ProviderUUID: providerUUID,
		IsSlim:       providerUser.IsSlim,
		Role:         role,
		LastLoginAt:  &now,
	}

	if err := s.db.Clauses(clause.OnConflict{
		Columns: []clause.Column{{Name: "provider_uuid"}},
		DoUpdates: clause.Assignments(map[string]any{
			"login":         user.Login,
			"is_slim":       user.IsSlim,
			"role":          user.Role,
			"last_login_at": now,
			"updated_at":    now,
		}),
	}).Create(&user).Error; err != nil {
		return LoginResult{}, err
	}

	var savedUser models.User
	if err := s.db.Where("provider_uuid = ?", providerUUID).First(&savedUser).Error; err != nil {
		return LoginResult{}, err
	}
	user = savedUser

	expiresAt := now.Add(s.tokenTTL)
	token, err := s.issueToken(user, expiresAt)
	if err != nil {
		return LoginResult{}, err
	}

	message := providerUser.Message
	if message == "" {
		message = "Успешная авторизация"
	}

	return LoginResult{
		Token:     token,
		ExpiresAt: expiresAt,
		User:      user,
		Message:   message,
		Policy:    policy.StatusFor(&user),
	}, nil
}

func (s Service) UserFromToken(ctx context.Context, tokenValue string) (models.User, error) {
	tokenValue = strings.TrimSpace(tokenValue)
	if tokenValue == "" {
		return models.User{}, errors.New("empty token")
	}

	claims := jwt.MapClaims{}
	token, err := jwt.ParseWithClaims(tokenValue, claims, func(token *jwt.Token) (any, error) {
		if _, ok := token.Method.(*jwt.SigningMethodHMAC); !ok {
			return nil, errors.New("unexpected signing method")
		}
		return s.jwtSecret, nil
	}, jwt.WithExpirationRequired())
	if err != nil || !token.Valid {
		if err == nil {
			err = errors.New("invalid token")
		}
		return models.User{}, err
	}

	userID, _ := claims["sub"].(string)
	if userID == "" {
		return models.User{}, errors.New("missing subject")
	}

	var user models.User
	if err := s.db.WithContext(ctx).Where("id = ?", userID).First(&user).Error; err != nil {
		return models.User{}, err
	}
	return user, nil
}

func (s Service) issueToken(user models.User, expiresAt time.Time) (string, error) {
	claims := jwt.MapClaims{
		"sub":           user.ID,
		"login":         user.Login,
		"provider_uuid": user.ProviderUUID,
		"role":          user.Role,
		"exp":           expiresAt.Unix(),
		"iat":           time.Now().UTC().Unix(),
	}

	return jwt.NewWithClaims(jwt.SigningMethodHS256, claims).SignedString(s.jwtSecret)
}

func (s Service) roleForLogin(ctx context.Context, login string, providerUUID string) (string, error) {
	var existingUser models.User
	if err := s.db.WithContext(ctx).
		Where("provider_uuid = ?", providerUUID).
		First(&existingUser).Error; err == nil && existingUser.IsPrivileged() {
		// Сохраняем уже назначенную привилегированную роль (admin/moderator),
		// чтобы повторный вход не сбрасывал её обратно в user.
		return existingUser.Role, nil
	} else if err != nil && !errors.Is(err, gorm.ErrRecordNotFound) {
		return "", err
	}

	if _, ok := s.adminLogins[normalizeLogin(login)]; ok {
		return "admin", nil
	}

	if s.autoAdminWhenEmpty {
		var adminCount int64
		if err := s.db.WithContext(ctx).
			Model(&models.User{}).
			Where("role = ?", "admin").
			Count(&adminCount).Error; err != nil {
			return "", err
		}
		if adminCount == 0 {
			return "admin", nil
		}
	}

	return "user", nil
}

func normalizeAdminLogins(logins []string) map[string]struct{} {
	normalized := make(map[string]struct{}, len(logins))
	for _, login := range logins {
		login = normalizeLogin(login)
		if login != "" {
			normalized[login] = struct{}{}
		}
	}
	return normalized
}

func normalizeLogin(login string) string {
	return strings.ToLower(strings.TrimSpace(login))
}
