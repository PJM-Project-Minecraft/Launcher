package anticheat

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"time"

	"launcher-backend/internal/models"

	"github.com/google/uuid"
	"gorm.io/gorm"
)

// SessionVerifier помечает игровую сессию прошедшей античит по nonce и умеет гасить
// её (kick). Реализуется yggdrasil.Store; интерфейс развязывает пакеты и упрощает тесты.
type SessionVerifier interface {
	MarkVerifiedByNonce(nonce string) bool
	InvalidateByNonce(nonce string) bool
}

// Service — бизнес-логика античита: handshake-init/confirm, запись детектов, выдача
// блэклиста, управление банами. Подпись launch-token делегируется TokenSigner.
type Service struct {
	db          *gorm.DB
	signer      TokenSigner
	autoBan     bool
	verifier    SessionVerifier
	agentPath    string
	nativeLinux  string
	nativeWin    string
	kickSeverity int
	notifier     Notifier
	now          func() time.Time
}

func NewService(db *gorm.DB, secret string, autoBan bool, verifier SessionVerifier, agentPath string) *Service {
	return &Service{
		db:           db,
		signer:       NewTokenSigner(secret),
		autoBan:      autoBan,
		verifier:     verifier,
		agentPath:    agentPath,
		kickSeverity: 7,
		now:          time.Now,
	}
}

// SetNotifier подключает отправку алертов о детектах (nil — алерты выключены).
func (s *Service) SetNotifier(n Notifier) {
	s.notifier = n
}

// SetKickSeverity задаёт порог серьёзности, с которого игрок кикается из игры.
func (s *Service) SetKickSeverity(severity int) {
	if severity > 0 {
		s.kickSeverity = severity
	}
}

// EvaluateKick решает, нужно ли кикнуть игрока за детект, и если да — гасит его
// игровую сессию (анти-reconnect). Реальный кик из запущенной игры делает агент,
// убивая JVM; здесь мы дополнительно закрываем сессию на сервере. Возвращает причину.
func (s *Service) EvaluateKick(claims LaunchClaims, severity int, dtype string) (bool, string) {
	// Кикаем только за реальную инъекцию/высокую серьёзность. Стартовые сигналы
	// (tamper "native-agent-missing", debugger) идут с низкой severity и лишь репортятся,
	// чтобы не выкидывать легитимных игроков, у кого нативный слой не поднялся.
	kick := severity >= s.kickSeverity || dtype == "inject"
	if !kick {
		return false, ""
	}
	if s.verifier != nil {
		s.verifier.InvalidateByNonce(claims.Nonce)
	}
	return true, dtype
}

// AgentPath — путь к agent.jar на диске (раздаётся лаунчеру для инжекта в JVM).
func (s *Service) AgentPath() string { return s.agentPath }

// SetNativePaths задаёт пути к нативным JVMTI-библиотекам по ОС.
func (s *Service) SetNativePaths(linux, win string) {
	s.nativeLinux = linux
	s.nativeWin = win
}

// NativePath возвращает путь к нативной библиотеке для запрошенной ОС (linux|windows).
func (s *Service) NativePath(os string) string {
	switch os {
	case "linux":
		return s.nativeLinux
	case "windows":
		return s.nativeWin
	default:
		return ""
	}
}

// Confirm завершает античит-handshake: валидирует launch-token и помечает связанную
// игровую сессию Verified (по nonce). Возвращает ошибку, если токен невалиден или
// nonce уже использован/неизвестен.
func (s *Service) Confirm(token string) error {
	claims, err := s.VerifyToken(token)
	if err != nil {
		return err
	}
	if s.verifier == nil || !s.verifier.MarkVerifiedByNonce(claims.Nonce) {
		return errors.New("session not found or already confirmed")
	}
	return nil
}

const (
	// launchTokenTTL — узкое окно между pre-launch init и стартом JVM/агентов.
	launchTokenTTL = 120 * time.Second
	// autoBanSeverity — порог серьёзности для авто-бана (если включён).
	autoBanSeverity = 8
)

// DetectionInput — единичное обнаружение, присланное лаунчером или агентом.
type DetectionInput struct {
	Source    string         `json:"source"`
	Type      string         `json:"type"`
	Signature string         `json:"signature"`
	Severity  int            `json:"severity"`
	Details   map[string]any `json:"details"`
}

// InitResult — ответ на handshake/init.
type InitResult struct {
	Allowed     bool   `json:"allowed"`
	Reason      string `json:"reason,omitempty"`
	LaunchToken string `json:"launchToken,omitempty"`
	Nonce       string `json:"nonce,omitempty"`
}

// InitHandshake проверяет баны, фиксирует HWID и pre-launch детекты, и при
// успехе выдаёт подписанный launch-token + nonce. Блок запуска = Allowed:false.
func (s *Service) InitHandshake(ctx context.Context, userUUID, login, hwidHash string, detections []DetectionInput) (InitResult, error) {
	now := s.now()

	if banned, reason := s.accountBanned(ctx, userUUID, now); banned {
		return InitResult{Allowed: false, Reason: reason}, nil
	}
	if hwidHash != "" {
		if banned, reason := s.hwidBanned(ctx, hwidHash, now); banned {
			return InitResult{Allowed: false, Reason: reason}, nil
		}
		if err := s.touchHwid(ctx, hwidHash, userUUID, login, now); err != nil {
			return InitResult{}, err
		}
	}

	// Фиксируем pre-launch детекты от лаунчера (скан процессов/файлов).
	for _, d := range detections {
		if err := s.recordDetection(ctx, userUUID, login, hwidHash, "", d, now); err != nil {
			return InitResult{}, err
		}
	}

	nonce := randomHex(16)
	token, err := s.signer.Sign(LaunchClaims{
		UUID:     userUUID,
		Login:    login,
		HwidHash: hwidHash,
		Nonce:    nonce,
		IssuedAt: now.Unix(),
		Expires:  now.Add(launchTokenTTL).Unix(),
	})
	if err != nil {
		return InitResult{}, err
	}
	return InitResult{Allowed: true, LaunchToken: token, Nonce: nonce}, nil
}

// VerifyToken проверяет launch-token (для аутентификации репортов и confirm).
func (s *Service) VerifyToken(token string) (LaunchClaims, error) {
	return s.signer.Verify(token, s.now())
}

// RecordDetection пишет обнаружение, аутентифицированное launch-token.
func (s *Service) RecordDetection(ctx context.Context, claims LaunchClaims, d DetectionInput) error {
	return s.recordDetection(ctx, claims.UUID, claims.Login, claims.HwidHash, claims.Nonce, d, s.now())
}

func (s *Service) recordDetection(ctx context.Context, userUUID, login, hwidHash, sessionID string, d DetectionInput, now time.Time) error {
	raw := ""
	if d.Details != nil {
		if b, err := json.Marshal(d.Details); err == nil {
			raw = string(b)
		}
	}
	source := d.Source
	if source == "" {
		source = "launcher"
	}
	rec := models.Detection{
		ID:        uuid.NewString(),
		UserUUID:  userUUID,
		Login:     login,
		HwidHash:  hwidHash,
		SessionID: sessionID,
		Source:    source,
		Type:      d.Type,
		Signature: d.Signature,
		Severity:  d.Severity,
		Raw:       raw,
		CreatedAt: now,
	}
	if err := s.db.WithContext(ctx).Create(&rec).Error; err != nil {
		return err
	}
	autoBanned := s.autoBan && d.Severity >= autoBanSeverity
	if autoBanned {
		_ = s.BanAccount(ctx, userUUID, login, "auto: "+d.Signature, "anticheat")
		if hwidHash != "" {
			_ = s.BanHwid(ctx, hwidHash, "auto: "+d.Signature, "anticheat")
		}
	}
	if s.notifier != nil {
		// Алерт не должен задерживать ответ лаунчеру/агенту.
		go s.notifier.NotifyDetection(rec, autoBanned)
	}
	return nil
}

// Blacklist возвращает включённые сигнатуры читов (для лаунчера и агентов).
func (s *Service) Blacklist(ctx context.Context) ([]models.CheatSignature, error) {
	var sigs []models.CheatSignature
	err := s.db.WithContext(ctx).Where("enabled = ?", true).Order("kind, pattern").Find(&sigs).Error
	return sigs, err
}

// --- Баны ---

func (s *Service) accountBanned(ctx context.Context, userUUID string, now time.Time) (bool, string) {
	var ban models.AccountBan
	err := s.db.WithContext(ctx).Where("user_uuid = ?", userUUID).First(&ban).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return false, ""
	}
	if err != nil {
		return false, ""
	}
	if ban.ExpiresAt != nil && now.After(*ban.ExpiresAt) {
		return false, ""
	}
	return true, banReason("Аккаунт заблокирован", ban.Reason)
}

func (s *Service) hwidBanned(ctx context.Context, hwidHash string, now time.Time) (bool, string) {
	var ban models.HwidBan
	err := s.db.WithContext(ctx).Where("hwid_hash = ?", hwidHash).First(&ban).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return false, ""
	}
	if err != nil {
		return false, ""
	}
	if ban.ExpiresAt != nil && now.After(*ban.ExpiresAt) {
		return false, ""
	}
	return true, banReason("Устройство заблокировано", ban.Reason)
}

func (s *Service) touchHwid(ctx context.Context, hwidHash, userUUID, login string, now time.Time) error {
	var h models.Hwid
	err := s.db.WithContext(ctx).Where("hash = ?", hwidHash).First(&h).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return s.db.WithContext(ctx).Create(&models.Hwid{
			Hash:          hwidHash,
			FirstUserUUID: userUUID,
			FirstLogin:    login,
			SeenCount:     1,
			FirstSeen:     now,
			LastSeen:      now,
		}).Error
	}
	if err != nil {
		return err
	}
	return s.db.WithContext(ctx).Model(&models.Hwid{}).Where("hash = ?", hwidHash).Updates(map[string]any{
		"seen_count": gorm.Expr("seen_count + 1"),
		"last_seen":  now,
	}).Error
}

func (s *Service) BanAccount(ctx context.Context, userUUID, login, reason, by string) error {
	ban := models.AccountBan{
		ID:        uuid.NewString(),
		UserUUID:  userUUID,
		Login:     login,
		Reason:    reason,
		BannedBy:  by,
		CreatedAt: s.now(),
	}
	return s.db.WithContext(ctx).Where("user_uuid = ?", userUUID).
		Assign(map[string]any{"reason": reason, "banned_by": by, "login": login}).
		FirstOrCreate(&ban).Error
}

func (s *Service) UnbanAccount(ctx context.Context, userUUID string) error {
	return s.db.WithContext(ctx).Where("user_uuid = ?", userUUID).Delete(&models.AccountBan{}).Error
}

func (s *Service) BanHwid(ctx context.Context, hwidHash, reason, by string) error {
	ban := models.HwidBan{
		ID:        uuid.NewString(),
		HwidHash:  hwidHash,
		Reason:    reason,
		BannedBy:  by,
		CreatedAt: s.now(),
	}
	return s.db.WithContext(ctx).Where("hwid_hash = ?", hwidHash).
		Assign(map[string]any{"reason": reason, "banned_by": by}).
		FirstOrCreate(&ban).Error
}

func (s *Service) UnbanHwid(ctx context.Context, hwidHash string) error {
	return s.db.WithContext(ctx).Where("hwid_hash = ?", hwidHash).Delete(&models.HwidBan{}).Error
}

// --- Admin-чтение ---

func (s *Service) ListDetections(ctx context.Context, limit int) ([]models.Detection, error) {
	if limit <= 0 || limit > 500 {
		limit = 100
	}
	var out []models.Detection
	err := s.db.WithContext(ctx).Order("created_at desc").Limit(limit).Find(&out).Error
	return out, err
}

func (s *Service) ListHwidBans(ctx context.Context) ([]models.HwidBan, error) {
	var out []models.HwidBan
	err := s.db.WithContext(ctx).Order("created_at desc").Find(&out).Error
	return out, err
}

func (s *Service) ListAccountBans(ctx context.Context) ([]models.AccountBan, error) {
	var out []models.AccountBan
	err := s.db.WithContext(ctx).Order("created_at desc").Find(&out).Error
	return out, err
}

// --- Сигнатуры (CRUD) ---

func (s *Service) ListSignatures(ctx context.Context) ([]models.CheatSignature, error) {
	var out []models.CheatSignature
	err := s.db.WithContext(ctx).Order("kind, pattern").Find(&out).Error
	return out, err
}

func (s *Service) CreateSignature(ctx context.Context, sig models.CheatSignature) (models.CheatSignature, error) {
	sig.ID = uuid.NewString()
	now := s.now()
	sig.CreatedAt = now
	sig.UpdatedAt = now
	err := s.db.WithContext(ctx).Create(&sig).Error
	return sig, err
}

func (s *Service) UpdateSignature(ctx context.Context, id string, updates map[string]any) error {
	updates["updated_at"] = s.now()
	return s.db.WithContext(ctx).Model(&models.CheatSignature{}).Where("id = ?", id).Updates(updates).Error
}

func (s *Service) DeleteSignature(ctx context.Context, id string) error {
	return s.db.WithContext(ctx).Where("id = ?", id).Delete(&models.CheatSignature{}).Error
}

func banReason(prefix, reason string) string {
	if reason == "" {
		return prefix
	}
	return prefix + ": " + reason
}

func randomHex(n int) string {
	buf := make([]byte, n)
	if _, err := rand.Read(buf); err != nil {
		return hex.EncodeToString([]byte(time.Now().String()))[:n*2]
	}
	return hex.EncodeToString(buf)
}
