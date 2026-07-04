// Package repo — слой доступа к данным Telegram-бота поверх общего GORM/Postgres.
// Заменяет прежний raw-SQL пакет db Telegram-бота; работает с launcher-backend/internal/models.
package repo

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"launcher-backend/internal/mcuuid"
	"launcher-backend/internal/models"

	"github.com/google/uuid"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

// ErrDuplicate — нарушение уникальности (логин/почта/uuid заняты).
var ErrDuplicate = errors.New("duplicate")

// --- FSM состояние диалога (перенесено из db.FlowState) ---

type FlowState int

const (
	FlowIdle FlowState = iota
	FlowLinkLogin
	FlowLinkPassword
	FlowLinkOtp
	FlowRegUsername
	FlowRegEmail
	FlowRegPassword
	FlowRegOtp
	FlowChangePwdOld
	FlowChangePwdWaitOtp
	FlowChangePwdNew
	FlowChangeEmailAsk
	FlowChangeEmailWaitOtp
	FlowTotpConfirm
	FlowTotpDisablePwd
	FlowTotpDisableOTP
	FlowAdminMenu
	FlowAdminSearch
	FlowAdminAwaitPick
	FlowAdminManaging
	FlowAdminAskNewEmail
)

var flowToString = map[FlowState]string{
	FlowIdle: "idle", FlowLinkLogin: "link_login", FlowLinkPassword: "link_password",
	FlowLinkOtp: "link_otp", FlowRegUsername: "reg_user", FlowRegEmail: "reg_email",
	FlowRegPassword: "reg_password", FlowRegOtp: "reg_otp", FlowChangePwdOld: "pwd_old",
	FlowChangePwdWaitOtp: "pwd_otp_wait", FlowChangePwdNew: "pwd_new", FlowChangeEmailAsk: "email_new",
	FlowChangeEmailWaitOtp: "email_otp", FlowTotpConfirm: "totp_confirm",
	FlowTotpDisablePwd: "totp_off_pwd", FlowTotpDisableOTP: "totp_off_otp", FlowAdminMenu: "admin_menu",
	FlowAdminSearch: "admin_search", FlowAdminAwaitPick: "admin_pick", FlowAdminManaging: "admin_manage",
	FlowAdminAskNewEmail: "admin_mail",
}

func (f FlowState) String() string {
	if s, ok := flowToString[f]; ok {
		return s
	}
	return "idle"
}

func ParseFlowState(raw string) (FlowState, error) {
	for st, s := range flowToString {
		if s == raw {
			return st, nil
		}
	}
	return FlowIdle, fmt.Errorf("unknown_state")
}

// DialoguePayload — контекст шага диалога (id пользователей теперь строковые uuid).
type DialoguePayload struct {
	Login              *string  `json:"login,omitempty"`
	OtpUserID          *string  `json:"otp_user_id,omitempty"`
	PendingNewEmail    *string  `json:"pending_new_email,omitempty"`
	PendingNewNickname *string  `json:"pending_new_nickname,omitempty"`
	AdminTargetID      *string  `json:"admin_target_id,omitempty"`
	AdminPickIDs       []string `json:"admin_pick_ids,omitempty"`

	PendingRegUsername *string `json:"pending_reg_username,omitempty"`
	PendingRegEmail    *string `json:"pending_reg_email,omitempty"`
	PendingRegPwdHash  *string `json:"pending_reg_pwd_hash,omitempty"`
	PendingRegOTPHash  *string `json:"pending_reg_otp_hash,omitempty"`
}

func EmptyPayload() DialoguePayload { return DialoguePayload{} }

// --- Пользователи ---

func firstUser(db *gorm.DB, q *gorm.DB) (*models.User, error) {
	var u models.User
	err := q.First(&u).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &u, nil
}

func FindUserByTelegram(ctx context.Context, db *gorm.DB, telegramID int64) (*models.User, error) {
	return firstUser(db, db.WithContext(ctx).Where("telegram_id = ?", telegramID))
}

// FindUserLogin ищет по логину или почте (логин = игровой ник при регистрации).
func FindUserLogin(ctx context.Context, db *gorm.DB, login string) (*models.User, error) {
	return firstUser(db, db.WithContext(ctx).Where("login = ? OR email = ?", login, login))
}

func FindUserByID(ctx context.Context, db *gorm.DB, id string) (*models.User, error) {
	return firstUser(db, db.WithContext(ctx).Where("id = ?", id))
}

// SearchUsers — до 10 совпадений по логину/почте/uuid.
func SearchUsers(ctx context.Context, db *gorm.DB, q string) ([]models.User, error) {
	like := "%" + q + "%"
	var out []models.User
	err := db.WithContext(ctx).
		Where("login LIKE ? OR email LIKE ? OR id = ?", like, like, q).
		Order("created_at DESC").Limit(10).Find(&out).Error
	return out, err
}

func BindTelegram(ctx context.Context, db *gorm.DB, userID string, telegramID int64, tgUsername *string) error {
	now := time.Now().UTC()
	updates := map[string]any{
		"telegram_id":        telegramID,
		"telegram_linked_at": now,
		"updated_at":         now,
	}
	if tgUsername != nil {
		updates["telegram_username"] = *tgUsername
	}
	return db.WithContext(ctx).Model(&models.User{}).Where("id = ?", userID).Updates(updates).Error
}

func SetPassword(ctx context.Context, db *gorm.DB, userID, bcryptHash string) error {
	return db.WithContext(ctx).Model(&models.User{}).Where("id = ?", userID).
		Updates(map[string]any{"password_hash": bcryptHash, "updated_at": time.Now().UTC()}).Error
}

func SetEmail(ctx context.Context, db *gorm.DB, userID, email string) error {
	return db.WithContext(ctx).Model(&models.User{}).Where("id = ?", userID).
		Updates(map[string]any{"email": email, "updated_at": time.Now().UTC()}).Error
}

func UpsertTotpSecretPending(ctx context.Context, db *gorm.DB, userID, secret string) error {
	return db.WithContext(ctx).Model(&models.User{}).Where("id = ?", userID).
		Updates(map[string]any{"totp_secret": secret, "totp_enabled": false, "updated_at": time.Now().UTC()}).Error
}

func SetTotpEnabled(ctx context.Context, db *gorm.DB, userID string, enabled bool) error {
	return db.WithContext(ctx).Model(&models.User{}).Where("id = ?", userID).
		Updates(map[string]any{"totp_enabled": enabled, "updated_at": time.Now().UTC()}).Error
}

func ClearTotp(ctx context.Context, db *gorm.DB, userID string) error {
	return db.WithContext(ctx).Model(&models.User{}).Where("id = ?", userID).
		Updates(map[string]any{"totp_secret": "", "totp_enabled": false, "updated_at": time.Now().UTC()}).Error
}

func UpdateUserAfterGMLLogin(ctx context.Context, db *gorm.DB, userID, ip, hardwareID string) error {
	now := time.Now().UTC()
	updates := map[string]any{"last_login_at": now, "updated_at": now}
	if ip != "" {
		updates["ip_address"] = ip
	}
	if hardwareID != "" && len(hardwareID) <= 512 {
		updates["hardware_id"] = hardwareID
	}
	return db.WithContext(ctx).Model(&models.User{}).Where("id = ?", userID).Updates(updates).Error
}

func IsUsernameTaken(ctx context.Context, db *gorm.DB, username string) (bool, error) {
	var n int64
	err := db.WithContext(ctx).Model(&models.User{}).Where("login = ?", username).Count(&n).Error
	return n > 0, err
}

func IsEmailTaken(ctx context.Context, db *gorm.DB, email string) (bool, error) {
	var n int64
	err := db.WithContext(ctx).Model(&models.User{}).Where("email = ?", email).Count(&n).Error
	return n > 0, err
}

// RegisterNewUser создаёт аккаунт с offline-UUID Minecraft (== ID == ProviderUUID) и возвращает его id.
func RegisterNewUser(ctx context.Context, db *gorm.DB, username, email, bcryptHash string) (string, error) {
	uidStr, err := mcuuid.OfflinePlayerUUIDString(username)
	if err != nil {
		return "", err
	}
	u := models.User{
		ID:           uidStr,
		Login:        username,
		ProviderUUID: uidStr,
		Email:        email,
		PasswordHash: bcryptHash,
		Role:         models.RoleUser,
	}
	if err := db.WithContext(ctx).Create(&u).Error; err != nil {
		if isUniqueViolation(err) {
			return "", ErrDuplicate
		}
		return "", err
	}
	return u.ID, nil
}

// --- Журналы и OTP ---

func InsertAuthLog(ctx context.Context, db *gorm.DB, userID *string, username, source string, ok bool, message *string) error {
	log := models.AuthLog{UserID: userID, Username: username, Source: source, Success: ok, CreatedAt: time.Now().UTC()}
	if message != nil {
		log.Message = *message
	}
	return db.WithContext(ctx).Create(&log).Error
}

func InsertOTP(ctx context.Context, db *gorm.DB, userID string, chatID int64, purpose, codeHash string, expiresAt time.Time) error {
	otp := models.TelegramOTP{
		ID: uuid.NewString(), UserID: userID, CodeHash: codeHash,
		ExpiresAt: expiresAt, TelegramChatID: chatID, Purpose: purpose,
	}
	return db.WithContext(ctx).Create(&otp).Error
}

// FindValidOTP возвращает id и хеш самого свежего непогашенного кода.
func FindValidOTP(ctx context.Context, db *gorm.DB, chatID int64, userID, purpose string) (id, codeHash string, ok bool, err error) {
	var otp models.TelegramOTP
	e := db.WithContext(ctx).
		Where("telegram_chat_id = ? AND user_id = ? AND purpose = ? AND consumed_at IS NULL AND expires_at > ?",
			chatID, userID, purpose, time.Now().UTC()).
		Order("expires_at DESC").First(&otp).Error
	if errors.Is(e, gorm.ErrRecordNotFound) {
		return "", "", false, nil
	}
	if e != nil {
		return "", "", false, e
	}
	return otp.ID, otp.CodeHash, true, nil
}

func ConsumeOTP(ctx context.Context, db *gorm.DB, id string) error {
	now := time.Now().UTC()
	return db.WithContext(ctx).Model(&models.TelegramOTP{}).Where("id = ?", id).
		Update("consumed_at", now).Error
}

func InsertAudit(ctx context.Context, db *gorm.DB, adminTelegramID *int64, adminUserID, targetUserID *string, action string, details *string) error {
	a := models.BotAuditLog{
		ID: uuid.NewString(), AdminTelegramID: adminTelegramID, AdminUserID: adminUserID,
		TargetUserID: targetUserID, Action: action, CreatedAt: time.Now().UTC(),
	}
	if details != nil {
		a.Details = *details
	}
	return db.WithContext(ctx).Create(&a).Error
}

// --- Диалог (FSM) ---

func ReadDialogue(ctx context.Context, db *gorm.DB, chatID int64) (FlowState, DialoguePayload, error) {
	var d models.BotDialogue
	err := db.WithContext(ctx).Where("chat_id = ?", chatID).First(&d).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return FlowIdle, EmptyPayload(), nil
	}
	if err != nil {
		return FlowIdle, EmptyPayload(), err
	}
	st, perr := ParseFlowState(d.State)
	if perr != nil {
		st = FlowIdle
	}
	var p DialoguePayload
	if d.Payload != "" {
		_ = json.Unmarshal([]byte(d.Payload), &p)
	}
	return st, p, nil
}

func SaveDialogue(ctx context.Context, db *gorm.DB, chatID int64, state FlowState, payload *DialoguePayload) error {
	raw, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	d := models.BotDialogue{ChatID: chatID, State: state.String(), Payload: string(raw), UpdatedAt: time.Now().UTC()}
	return db.WithContext(ctx).Clauses(clause.OnConflict{
		Columns:   []clause.Column{{Name: "chat_id"}},
		DoUpdates: clause.AssignmentColumns([]string{"state", "payload", "updated_at"}),
	}).Create(&d).Error
}

func ClearDialogue(ctx context.Context, db *gorm.DB, chatID int64) error {
	return db.WithContext(ctx).Where("chat_id = ?", chatID).Delete(&models.BotDialogue{}).Error
}

// --- Меню-сообщение бота ---

func SaveMenuMessage(ctx context.Context, db *gorm.DB, chatID int64, messageID int) error {
	m := models.BotMenuMessage{ChatID: chatID, MessageID: messageID, UpdatedAt: time.Now().UTC()}
	return db.WithContext(ctx).Clauses(clause.OnConflict{
		Columns:   []clause.Column{{Name: "chat_id"}},
		DoUpdates: clause.AssignmentColumns([]string{"message_id", "updated_at"}),
	}).Create(&m).Error
}

func ReadMenuMessage(ctx context.Context, db *gorm.DB, chatID int64) (int, error) {
	var m models.BotMenuMessage
	err := db.WithContext(ctx).Where("chat_id = ?", chatID).First(&m).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return 0, nil
	}
	if err != nil {
		return 0, err
	}
	return m.MessageID, nil
}

// --- Заявки на сброс пароля («забыл пароль») ---

// CreatePwdReset создаёт заявку; если у пользователя уже есть pending-заявка,
// возвращает её id и created=false (повторная отправка не плодит дубли).
func CreatePwdReset(ctx context.Context, db *gorm.DB, userID string, chatID int64) (uint, bool, error) {
	var existing models.BotPasswordReset
	err := db.WithContext(ctx).
		Where("user_id = ? AND status = ?", userID, models.PwdResetPending).
		First(&existing).Error
	if err == nil {
		return existing.ID, false, nil
	}
	if !errors.Is(err, gorm.ErrRecordNotFound) {
		return 0, false, err
	}
	req := models.BotPasswordReset{UserID: userID, ChatID: chatID, Status: models.PwdResetPending}
	if err := db.WithContext(ctx).Create(&req).Error; err != nil {
		return 0, false, err
	}
	return req.ID, true, nil
}

func GetPwdReset(ctx context.Context, db *gorm.DB, id uint) (*models.BotPasswordReset, error) {
	var r models.BotPasswordReset
	err := db.WithContext(ctx).Where("id = ?", id).First(&r).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &r, nil
}

// DecidePwdReset переводит pending-заявку в approved/rejected; возвращает false,
// если заявка уже решена (защита от двойного клика и гонки двух админов).
func DecidePwdReset(ctx context.Context, db *gorm.DB, id uint, status, decidedBy string) (bool, error) {
	res := db.WithContext(ctx).Model(&models.BotPasswordReset{}).
		Where("id = ? AND status = ?", id, models.PwdResetPending).
		Updates(map[string]any{"status": status, "decided_by": decidedBy, "updated_at": time.Now().UTC()})
	return res.RowsAffected > 0, res.Error
}

func ListPendingPwdResets(ctx context.Context, db *gorm.DB) ([]models.BotPasswordReset, error) {
	var out []models.BotPasswordReset
	err := db.WithContext(ctx).
		Where("status = ?", models.PwdResetPending).
		Order("created_at ASC").Limit(20).Find(&out).Error
	return out, err
}

// ListPrivilegedWithTelegram — админы/модераторы с привязанным Telegram
// (кандидаты на уведомление о заявках).
func ListPrivilegedWithTelegram(ctx context.Context, db *gorm.DB) ([]models.User, error) {
	var out []models.User
	err := db.WithContext(ctx).
		Where("role IN ? AND telegram_id IS NOT NULL", []string{models.RoleAdmin, models.RoleModerator}).
		Find(&out).Error
	return out, err
}

// UserStats — сводные счётчики для админ-панели бота.
type UserStats struct {
	Total     int64
	Linked    int64
	TotpOn    int64
	Banned    int64
	NewWeek   int64
	PwdReqs   int64
}

func FetchUserStats(ctx context.Context, db *gorm.DB) (UserStats, error) {
	var st UserStats
	q := func(dst *int64, tx *gorm.DB) error { return tx.Count(dst).Error }
	base := func() *gorm.DB { return db.WithContext(ctx).Model(&models.User{}) }
	if err := q(&st.Total, base()); err != nil {
		return st, err
	}
	if err := q(&st.Linked, base().Where("telegram_id IS NOT NULL")); err != nil {
		return st, err
	}
	if err := q(&st.TotpOn, base().Where("totp_enabled = ?", true)); err != nil {
		return st, err
	}
	if err := q(&st.Banned, base().Where("is_banned = ? OR is_hwid_banned = ?", true, true)); err != nil {
		return st, err
	}
	weekAgo := time.Now().UTC().AddDate(0, 0, -7)
	if err := q(&st.NewWeek, base().Where("created_at >= ?", weekAgo)); err != nil {
		return st, err
	}
	if err := q(&st.PwdReqs, db.WithContext(ctx).Model(&models.BotPasswordReset{}).
		Where("status = ?", models.PwdResetPending)); err != nil {
		return st, err
	}
	return st, nil
}

// isUniqueViolation распознаёт конфликт уникальности для Postgres (23505) и SQLite.
func isUniqueViolation(err error) bool {
	if err == nil {
		return false
	}
	msg := err.Error()
	return containsAny(msg, "23505", "duplicate key", "UNIQUE constraint failed", "Duplicate entry")
}

func containsAny(s string, subs ...string) bool {
	for _, sub := range subs {
		if len(sub) > 0 && indexOf(s, sub) >= 0 {
			return true
		}
	}
	return false
}

func indexOf(s, sub string) int {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return i
		}
	}
	return -1
}
