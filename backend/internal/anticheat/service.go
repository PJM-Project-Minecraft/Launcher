package anticheat

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"log/slog"
	"strings"
	"sync"
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
	IsActiveByNonce(nonce string) bool
	// TouchByNonce продлевает игровую сессию (sliding TTL): heartbeat — сигнал живости
	// игры, без которого 15-мин TTL сессии истекает прямо во время игры.
	TouchByNonce(nonce string) bool
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

	authlibPath        string // путь к authlib-injector.jar (для SHA-манифеста)
	requireAttestation bool   // true — confirm без валидного proof отклоняется

	recentMu sync.Mutex
	recent   map[string]time.Time // дедуп: ключ детекта -> время последней записи

	shaMu      sync.Mutex
	shaEntries map[string]shaEntry // кэш SHA-256 артефактов по пути (инвалидация по mtime+size)

	hbMu       sync.Mutex
	heartbeats map[string]time.Time // nonce -> последний heartbeat (живость агента)
	hbTimeout  time.Duration        // без heartbeat дольше hbTimeout → сессия гасится reaper'ом
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
		recent:       make(map[string]time.Time),
		shaEntries:   make(map[string]shaEntry),
		heartbeats:   make(map[string]time.Time),
		hbTimeout:    90 * time.Second,
	}
}

// SetHeartbeatTimeout задаёт окно живости агента (без heartbeat дольше — kick через reaper).
func (s *Service) SetHeartbeatTimeout(d time.Duration) {
	if d > 0 {
		s.hbTimeout = d
	}
}

// SetAuthlibPath задаёт путь к authlib-injector.jar — он тоже инжектится как
// -javaagent, поэтому его SHA включается в манифест целостности.
func (s *Service) SetAuthlibPath(p string) { s.authlibPath = p }

// SetRequireAttestation включает жёсткую проверку proof в confirm. Включать ТОЛЬКО
// после раздачи лаунчера с attestation-proof (mandatory-bump): при true старый клиент
// без валидного proof не пройдёт confirm → не получит verified-сессию. По умолчанию
// false (transition): расхождения логируются, но запуск не блокируется.
func (s *Service) SetRequireAttestation(v bool) { s.requireAttestation = v }

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
// ConfirmProof — доказательство присутствия живого агента, присылаемое в confirm.
// Агент вычисляет это ВНУТРИ JVM; backend сверяет с challenge из токена и манифестом.
type ConfirmProof struct {
	Challenge     string `json:"challenge"`     // эхо challenge из claims (свежесть/привязка к сессии)
	AgentSha256   string `json:"agentSha256"`   // self-hash jar агента (должен совпасть с манифестом)
	NativePresent bool   `json:"nativePresent"` // нативный JVMTI-агент реально загрузился
	ForeignAgents bool   `json:"foreignAgents"` // обнаружен посторонний -javaagent/-agentpath
}

func (s *Service) Confirm(token string, proof ConfirmProof) error {
	claims, err := s.VerifyToken(token)
	if err != nil {
		return err
	}
	if perr := s.verifyProof(claims, proof); perr != nil {
		if s.requireAttestation {
			return perr // жёсткий режим: без валидного proof не верифицируем сессию
		}
		// Transition: фиксируем будущий отказ, но пускаем (пока не раздан новый лаунчер).
		slog.Warn("anticheat: attestation would fail (transition mode)", "login", claims.Login, "reason", perr)
	}
	if s.verifier == nil || !s.verifier.MarkVerifiedByNonce(claims.Nonce) {
		return errors.New("session not found or already confirmed")
	}
	// Трекинг живости стартует после успешного confirm.
	s.touchHeartbeat(claims.Nonce)
	return nil
}

// verifyProof проверяет доказательство присутствия агента: эхо challenge, наличие
// нативного слоя, отсутствие посторонних агентов и совпадение self-hash с манифестом.
// Honest: полностью клиентское доказательство не доказывает исполнение на 100% (см. план,
// остаточная дыра) — реальный замок это серверный in-game-handshake (P5). Здесь поднимаем
// планку: confirm обязан предъявить связный, согласованный с challenge и манифестом proof.
func (s *Service) verifyProof(claims LaunchClaims, p ConfirmProof) error {
	if claims.Challenge == "" || p.Challenge != claims.Challenge {
		return errors.New("attestation: challenge mismatch")
	}
	if !p.NativePresent {
		return errors.New("attestation: native agent absent")
	}
	if p.ForeignAgents {
		return errors.New("attestation: foreign agent present")
	}
	if want := s.cachedSha(s.agentPath); want != "" && !strings.EqualFold(p.AgentSha256, want) {
		return errors.New("attestation: agent hash mismatch")
	}
	return nil
}

// touchHeartbeat фиксирует время живости агента по nonce.
func (s *Service) touchHeartbeat(nonce string) {
	if nonce == "" {
		return
	}
	s.hbMu.Lock()
	s.heartbeats[nonce] = s.now()
	s.hbMu.Unlock()
}

// Heartbeat обрабатывает пинг агента: обновляет живость и сообщает, нужно ли кикнуть
// (сессия погашена detect'ом → IsActiveByNonce=false) и текущую версию блэклиста
// (агент по её изменению ре-фетчит правила).
func (s *Service) Heartbeat(ctx context.Context, claims LaunchClaims) (kick bool, blacklistVersion int64) {
	s.touchHeartbeat(claims.Nonce)
	// Продлеваем игровую сессию: heartbeat доказывает, что игра ещё запущена, и держит
	// yggdrasil-токен живым на весь сеанс (иначе реконнект после 15 мин → invalid session).
	// No-op, если сессию уже погасили (detect-kick) — IsActiveByNonce ниже вернёт kick.
	if s.verifier != nil {
		s.verifier.TouchByNonce(claims.Nonce)
	}
	active := s.verifier == nil || s.verifier.IsActiveByNonce(claims.Nonce)
	return !active, s.BlacklistVersion(ctx)
}

// reapStale ловит сессии, по которым давно (дольше hbTimeout) не было heartbeat от
// агента, и шлёт по ним МЯГКИЙ детект (алерт), НЕ гася сессию. Раньше reaper гасил
// сессию (InvalidateByNonce), но heartbeat-тред агента мог тихо умереть в модовом
// окружении → честного игрока выкидывало «Недействительной сессией» при реконнекте.
// Живость игровой сессии теперь держит keepalive от лаунчера; молчание агента —
// лишь повод присмотреться. Реальный чит гасит сессию отдельно (detect-kick).
// Дедуп: nonce снимается с трекинга живости, так что алерт уходит один раз.
// Время инъектируется для детерминированных тестов.
func (s *Service) reapStale(now time.Time) {
	s.hbMu.Lock()
	var silent []string
	for nonce, last := range s.heartbeats {
		if now.Sub(last) > s.hbTimeout {
			silent = append(silent, nonce)
			delete(s.heartbeats, nonce)
		}
	}
	s.hbMu.Unlock()
	for _, nonce := range silent {
		// Алертим, только если сессия ещё активна (игра предположительно идёт, а агент
		// замолк — вот это подозрительно). Если сессия уже погашена (игрок закрыл игру →
		// лаунчер invalidate, или detect-kick), молчание агента ожидаемо → тихо снимаем
		// с трекинга без ложного алерта.
		if s.verifier == nil || !s.verifier.IsActiveByNonce(nonce) {
			continue
		}
		slog.Warn("anticheat: agent heartbeat silent (мягкий детект, сессию не гасим)",
			"nonce", nonce, "timeout", s.hbTimeout)
		if s.notifier != nil {
			s.notifier.NotifyAgentSilent(nonce)
		}
	}
}

// StartHeartbeatReaper запускает фоновый reaper (вызывать один раз из main.go).
func (s *Service) StartHeartbeatReaper(interval time.Duration) {
	go func() {
		for range time.Tick(interval) {
			s.reapStale(s.now())
		}
	}()
}

// BlacklistVersion — версия блэклиста (max updated_at включённых сигнатур, Unix-сек).
// 0 — блэклист пуст. Агент/лаунчер сравнивают её, чтобы понять, надо ли ре-фетчить.
// Считаем в Go (а не SQL max): GORM надёжно мапит колонку в time.Time на SQLite и Postgres,
// тогда как скан max(updated_at) в sql.NullTime на SQLite-тексте ненадёжен.
func (s *Service) BlacklistVersion(ctx context.Context) int64 {
	var sigs []models.CheatSignature
	if err := s.db.WithContext(ctx).Where("enabled = ?", true).Select("updated_at").Find(&sigs).Error; err != nil {
		return 0
	}
	var max int64
	for _, sig := range sigs {
		if u := sig.UpdatedAt.Unix(); u > max {
			max = u
		}
	}
	return max
}

// RuleSignature — облегчённая запись блэклиста для агента (без id/служебных полей).
type RuleSignature struct {
	Kind     string `json:"kind"`
	Pattern  string `json:"pattern"`
	Severity int    `json:"severity"`
}

// RulesResponse — ответ /rules: версия + включённые сигнатуры для рантайм-скана агентом.
type RulesResponse struct {
	Version    int64           `json:"version"`
	Signatures []RuleSignature `json:"signatures"`
}

// Rules отдаёт текущий блэклист агенту (по launch-token, без JWT).
func (s *Service) Rules(ctx context.Context) (RulesResponse, error) {
	sigs, err := s.Blacklist(ctx)
	if err != nil {
		return RulesResponse{}, err
	}
	out := RulesResponse{Version: s.BlacklistVersion(ctx), Signatures: make([]RuleSignature, 0, len(sigs))}
	for _, sig := range sigs {
		out.Signatures = append(out.Signatures, RuleSignature{Kind: sig.Kind, Pattern: sig.Pattern, Severity: sig.Severity})
	}
	return out, nil
}

const (
	// launchTokenTTL — узкое окно между pre-launch init и стартом JVM/агентов.
	launchTokenTTL = 120 * time.Second
	// autoBanSeverity — порог серьёзности для авто-бана (если включён).
	autoBanSeverity = 8
	// defaultDetectionSeverity — severity сигнатурного детекта, не совпавшего с блэклистом.
	defaultDetectionSeverity = 5
	// tempBanDuration — длительность первого (временного) авто-бана до эскалации в перманент.
	tempBanDuration = 7 * 24 * time.Hour
	// detectDedupWindow — окно, в котором повторный идентичный детект не пишется снова.
	detectDedupWindow = 30 * time.Second
)

// systemSeverity — СЕРВЕРНАЯ (не клиентская) серьёзность для системных типов детекта.
// Клиент не может занизить severity реальной инъекции/тампера: значение берётся отсюда,
// а не из тела запроса. Сигнатурные типы (process|class|jar|file) — из блэклиста.
var systemSeverity = map[string]int{
	"inject":   9,
	"attach":   9,
	"tamper":   8,
	"debugger": 6,
}

// normalizeSource валидирует источник детекта по whitelist (анти-спуф source).
// Пустой источник трактуется как "launcher" (pre-launch скан лаунчера), невалидный —
// как "unknown" (запись сохраняется, но не выдаёт себя за доверенный слой).
func normalizeSource(src string) string {
	switch src {
	case "launcher", "java", "native":
		return src
	case "":
		return "launcher"
	default:
		return "unknown"
	}
}

// resolveSeverity вычисляет серьёзность детекта НА СЕРВЕРЕ, игнорируя клиентское
// значение. Системные типы — из systemSeverity; сигнатурные — максимальная severity
// совпавшей записи блэклиста; иначе — defaultDetectionSeverity.
func (s *Service) resolveSeverity(ctx context.Context, dtype, signature string) int {
	if sv, ok := systemSeverity[dtype]; ok {
		return sv
	}
	if sv := s.signatureSeverity(ctx, dtype, signature); sv > 0 {
		return sv
	}
	return defaultDetectionSeverity
}

// signatureSeverity ищет в блэклисте включённые сигнатуры заданного kind, чей Pattern
// является подстрокой signature, и возвращает максимальную их severity (0 — не найдено).
func (s *Service) signatureSeverity(ctx context.Context, kind, signature string) int {
	if signature == "" {
		return 0
	}
	sig := strings.ToLower(signature)
	var rows []models.CheatSignature
	if err := s.db.WithContext(ctx).Where("enabled = ? AND kind = ?", true, kind).Find(&rows).Error; err != nil {
		return 0
	}
	best := 0
	for _, r := range rows {
		p := strings.ToLower(r.Pattern)
		if p != "" && strings.Contains(sig, p) && r.Severity > best {
			best = r.Severity
		}
	}
	return best
}

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
	Challenge   string `json:"challenge,omitempty"` // агент возвращает его в confirm-proof
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
		if _, err := s.recordDetection(ctx, userUUID, login, hwidHash, "", d, now); err != nil {
			return InitResult{}, err
		}
	}

	nonce := randomHex(16)
	challenge := randomHex(16)
	token, err := s.signer.Sign(LaunchClaims{
		UUID:      userUUID,
		Login:     login,
		HwidHash:  hwidHash,
		Nonce:     nonce,
		Challenge: challenge,
		IssuedAt:  now.Unix(),
		Expires:   now.Add(launchTokenTTL).Unix(),
	})
	if err != nil {
		return InitResult{}, err
	}
	return InitResult{Allowed: true, LaunchToken: token, Nonce: nonce, Challenge: challenge}, nil
}

// VerifyToken проверяет launch-token (для аутентификации репортов и confirm).
func (s *Service) VerifyToken(token string) (LaunchClaims, error) {
	return s.signer.Verify(token, s.now())
}

// RecordDetection пишет обнаружение, аутентифицированное launch-token, и возвращает
// СЕРВЕРНУЮ серьёзность детекта (используется для решения о kick). Клиентская
// severity из запроса игнорируется — её нельзя занизить.
func (s *Service) RecordDetection(ctx context.Context, claims LaunchClaims, d DetectionInput) (int, error) {
	return s.recordDetection(ctx, claims.UUID, claims.Login, claims.HwidHash, claims.Nonce, d, s.now())
}

func (s *Service) recordDetection(ctx context.Context, userUUID, login, hwidHash, sessionID string, d DetectionInput, now time.Time) (int, error) {
	severity := s.resolveSeverity(ctx, d.Type, d.Signature)
	// Дедуп: одинаковый детект в пределах окна не пишем повторно, но severity всё равно
	// возвращаем — kick-решение должно срабатывать и на «застрявшем»/спамящем агенте.
	if s.isDuplicate(userUUID, sessionID, d.Type, d.Signature, now) {
		return severity, nil
	}
	raw := ""
	if d.Details != nil {
		if b, err := json.Marshal(d.Details); err == nil {
			raw = string(b)
		}
	}
	rec := models.Detection{
		ID:        uuid.NewString(),
		UserUUID:  userUUID,
		Login:     login,
		HwidHash:  hwidHash,
		SessionID: sessionID,
		Source:    normalizeSource(d.Source),
		Type:      d.Type,
		Signature: d.Signature,
		Severity:  severity,
		Raw:       raw,
		CreatedAt: now,
	}
	if err := s.db.WithContext(ctx).Create(&rec).Error; err != nil {
		return severity, err
	}
	autoBanned := s.autoBan && severity >= autoBanSeverity
	if autoBanned {
		s.autoBanEscalated(ctx, userUUID, login, hwidHash, d.Signature, now)
	}
	if s.notifier != nil {
		// Алерт не должен задерживать ответ лаунчеру/агенту.
		go s.notifier.NotifyDetection(rec, autoBanned)
	}
	return severity, nil
}

// isDuplicate возвращает true, если идентичный детект уже писался в пределах
// detectDedupWindow (защита от флуда «застрявшего» агента). Заодно чистит протухшие
// записи, чтобы карта не росла бесконечно.
func (s *Service) isDuplicate(userUUID, sessionID, dtype, signature string, now time.Time) bool {
	key := userUUID + "|" + sessionID + "|" + dtype + "|" + signature
	s.recentMu.Lock()
	defer s.recentMu.Unlock()
	for k, t := range s.recent {
		if now.Sub(t) > detectDedupWindow {
			delete(s.recent, k)
		}
	}
	if t, ok := s.recent[key]; ok && now.Sub(t) <= detectDedupWindow {
		return true
	}
	s.recent[key] = now
	return false
}

// autoBanEscalated применяет эскалацию авто-бана: первое нарушение — временный бан
// (tempBanDuration), повторное (для аккаунта/HWID уже есть бан-запись) — перманентный.
func (s *Service) autoBanEscalated(ctx context.Context, userUUID, login, hwidHash, signature string, now time.Time) {
	var expiry *time.Time
	if !s.hasPriorBan(ctx, userUUID, hwidHash) {
		t := now.Add(tempBanDuration)
		expiry = &t
	}
	reason := "auto: " + signature
	_ = s.banAccount(ctx, userUUID, login, reason, "anticheat", expiry)
	if hwidHash != "" {
		_ = s.banHwid(ctx, hwidHash, reason, "anticheat", expiry)
	}
}

// hasPriorBan сообщает, банился ли уже этот аккаунт или HWID (запись существует,
// даже если истекла) — сигнал для эскалации в перманентный бан.
func (s *Service) hasPriorBan(ctx context.Context, userUUID, hwidHash string) bool {
	var n int64
	s.db.WithContext(ctx).Model(&models.AccountBan{}).Where("user_uuid = ?", userUUID).Count(&n)
	if n > 0 {
		return true
	}
	if hwidHash != "" {
		s.db.WithContext(ctx).Model(&models.HwidBan{}).Where("hwid_hash = ?", hwidHash).Count(&n)
		if n > 0 {
			return true
		}
	}
	return false
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
	return s.banAccount(ctx, userUUID, login, reason, by, nil)
}

// banAccount апсертит бан аккаунта. expiresAt=nil — перманентный, иначе временный.
// Select(...) форсит запись expires_at даже при nil (эскалация temp→perm обнуляет срок).
func (s *Service) banAccount(ctx context.Context, userUUID, login, reason, by string, expiresAt *time.Time) error {
	var existing models.AccountBan
	err := s.db.WithContext(ctx).Where("user_uuid = ?", userUUID).First(&existing).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return s.db.WithContext(ctx).Create(&models.AccountBan{
			ID: uuid.NewString(), UserUUID: userUUID, Login: login,
			Reason: reason, BannedBy: by, CreatedAt: s.now(), ExpiresAt: expiresAt,
		}).Error
	}
	if err != nil {
		return err
	}
	return s.db.WithContext(ctx).Model(&existing).
		Select("reason", "banned_by", "login", "expires_at").
		Updates(map[string]any{"reason": reason, "banned_by": by, "login": login, "expires_at": expiresAt}).Error
}

func (s *Service) UnbanAccount(ctx context.Context, userUUID string) error {
	return s.db.WithContext(ctx).Where("user_uuid = ?", userUUID).Delete(&models.AccountBan{}).Error
}

func (s *Service) BanHwid(ctx context.Context, hwidHash, reason, by string) error {
	return s.banHwid(ctx, hwidHash, reason, by, nil)
}

// banHwid апсертит аппаратный бан. expiresAt=nil — перманентный, иначе временный.
func (s *Service) banHwid(ctx context.Context, hwidHash, reason, by string, expiresAt *time.Time) error {
	var existing models.HwidBan
	err := s.db.WithContext(ctx).Where("hwid_hash = ?", hwidHash).First(&existing).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return s.db.WithContext(ctx).Create(&models.HwidBan{
			ID: uuid.NewString(), HwidHash: hwidHash,
			Reason: reason, BannedBy: by, CreatedAt: s.now(), ExpiresAt: expiresAt,
		}).Error
	}
	if err != nil {
		return err
	}
	return s.db.WithContext(ctx).Model(&existing).
		Select("reason", "banned_by", "expires_at").
		Updates(map[string]any{"reason": reason, "banned_by": by, "expires_at": expiresAt}).Error
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
