package anticheat

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"log/slog"
	"regexp"
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
	// LauncherActive — лаунчер недавно слал keepalive по nonce (игра точно запущена).
	// Это надёжный сигнал «игра идёт» от стабильного лаунчера: по нему reaper отличает
	// убийство агента в живой игре (алерт) от обычного закрытия игры (тишина).
	LauncherActive(nonce string) bool
}

// Service — бизнес-логика античита: handshake-init/confirm, запись детектов, выдача
// блэклиста, управление банами. Подпись launch-token делегируется TokenSigner.
type Service struct {
	db           *gorm.DB
	signer       TokenSigner
	autoBan      bool
	verifier     SessionVerifier
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
func (s *Service) EvaluateKick(claims LaunchClaims, severity int, confidence, dtype string) (bool, string) {
	// inject — всегда kick (явная инъекция / ghost-client). Прочее кикаем по severity,
	// но ТОЛЬКО hard-детекты: soft-эвристики (tamper "native-agent-missing", debugger,
	// substring-матчи) не должны выкидывать легитимного игрока, даже если их серверная
	// severity высока (tamper=8). Это закрывает ложный кик игрока без нативного слоя.
	kick := dtype == "inject" || (severity >= s.kickSeverity && confidence == "hard")
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
		// Алертим, только если игра ТОЧНО идёт (лаунчер свежо keepalive'ит), а агент при
		// этом замолк — его убили в живой игре. Если keepalive лаунчера тоже пропал, игра
		// просто закрыта → молчание агента ожидаемо → тихо снимаем с трекинга без ложного
		// алерта. Сессию проверяем заодно (detect-kick мог её уже погасить).
		// У лаунчеров без keepalive (старые версии) LauncherActive всегда false → тишина:
		// без сигнала живости лаунчера закрытие игры неотличимо от убийства агента.
		if s.verifier == nil || !s.verifier.IsActiveByNonce(nonce) || !s.verifier.LauncherActive(nonce) {
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
// MatchType и Hash — аддитивные поля: старые клиенты (Java parsePatterns берёт только
// "pattern", Rust Signature имеет serde(default)) их игнорируют и не ломаются.
type RuleSignature struct {
	Kind      string `json:"kind"`
	Pattern   string `json:"pattern"`
	Severity  int    `json:"severity"`
	MatchType string `json:"matchType"`
	Hash      string `json:"hash"`
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
		out.Signatures = append(out.Signatures, RuleSignature{
			Kind:      sig.Kind,
			Pattern:   sig.Pattern,
			Severity:  sig.Severity,
			MatchType: effectiveMatchType(sig.MatchType),
			Hash:      sig.HashHex,
		})
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

// detectionConfidence делит детект на hard (высокая уверенность — кандидат на
// авто-бан/кик) и soft (эвристика, возможен ложняк — пишется, но сам не банит и не
// кикает; разбирается в review-очереди). hard — системная инъекция/attach (вкл.
// illegal-class-name и foreign-agent, приходящие как type "inject") и точный
// сигнатурный матч (exact|word|regex|hash). Substring-матч и стартовые эвристики
// (tamper, debugger, module-unknown, ld-preload) — soft: легальный игрок не должен
// от них пострадать.
func detectionConfidence(dtype, matchType string) string {
	switch dtype {
	case "inject", "attach":
		return "hard"
	}
	switch matchType {
	case "exact", "word", "regex", "hash", "string-literal":
		return "hard"
	}
	return "soft"
}

// resolveSeverity — серверная severity детекта (без match_type/hash; для системных типов
// и прямого использования в тестах). Обёртка над resolveDetection.
func (s *Service) resolveSeverity(ctx context.Context, dtype, signature string) int {
	sev, _ := s.resolveDetection(ctx, dtype, signature, "")
	return sev
}

// resolveDetection вычисляет серверную severity И match_type сработавшей сигнатуры,
// игнорируя клиентское значение. Системные типы — из systemSeverity (match_type пустой,
// confidence по dtype); hash байткода (если прислан) — из hash-сигнатуры; сигнатурные —
// из сильнейшего совпадения блэклиста по Pattern; иначе — defaultDetectionSeverity.
func (s *Service) resolveDetection(ctx context.Context, dtype, signature, hash string) (int, string) {
	if sv, ok := systemSeverity[dtype]; ok {
		return sv, ""
	}
	// Хеш-матч байткода (Фаза 2): если клиент прислал hash, ищем hash-сигнатуру —
	// она матчит независимо от имени класса, поэтому бьёт обфускацию имён.
	if hash != "" {
		if sv := s.hashSeverity(ctx, dtype, hash); sv > 0 {
			return sv, "hash"
		}
	}
	if sv, mt := s.signatureMatch(ctx, dtype, signature); sv > 0 {
		return sv, mt
	}
	return defaultDetectionSeverity, ""
}

// hashSeverity ищет включённую hash-сигнатуру заданного kind с совпадающим HashHex и
// возвращает её максимальную severity (0 — не найдено).
func (s *Service) hashSeverity(ctx context.Context, kind, hash string) int {
	var rows []models.CheatSignature
	if err := s.db.WithContext(ctx).
		Where("enabled = ? AND kind = ? AND match_type = ? AND hash_hex = ?", true, kind, "hash", hash).
		Find(&rows).Error; err != nil {
		return 0
	}
	best := 0
	for _, r := range rows {
		if r.Severity > best {
			best = r.Severity
		}
	}
	return best
}

// detectionHash извлекает SHA-256 байткода из Details детекта (поле "hash"), если есть.
func detectionHash(details map[string]any) string {
	if details == nil {
		return ""
	}
	if h, ok := details["hash"].(string); ok {
		return h
	}
	return ""
}

// signatureMatch ищет в блэклисте включённые сигнатуры заданного kind, совпавшие с
// сигналом по своему MatchType, и возвращает максимальную severity и MatchType
// сильнейшего совпадения (0, "" — не найдено).
func (s *Service) signatureMatch(ctx context.Context, kind, signature string) (int, string) {
	if signature == "" {
		return 0, ""
	}
	sig := strings.ToLower(signature)
	var rows []models.CheatSignature
	if err := s.db.WithContext(ctx).Where("enabled = ? AND kind = ?", true, kind).Find(&rows).Error; err != nil {
		return 0, ""
	}
	bestSev, bestMatch := 0, ""
	for _, r := range rows {
		if matchPattern(r.MatchType, r.Pattern, sig) && r.Severity > bestSev {
			bestSev = r.Severity
			bestMatch = effectiveMatchType(r.MatchType)
		}
	}
	return bestSev, bestMatch
}

// effectiveMatchType нормализует пустой MatchType к "substring" (обратная совместимость).
func effectiveMatchType(mt string) string {
	if mt == "" {
		return "substring"
	}
	return mt
}

// matchPattern сопоставляет одну сигнатуру блэклиста с сигналом детекта по её MatchType.
// signatureLower — сигнал уже в нижнем регистре. Хэш-матч (MatchType "hash") здесь не
// обрабатывается — он по HashHex, а не по Pattern (см. Фаза 2).
func matchPattern(matchType, pattern, signatureLower string) bool {
	if pattern == "" {
		return false
	}
	p := strings.ToLower(pattern)
	switch matchType {
	case "exact":
		return signatureLower == p
	case "word":
		return matchesWord(signatureLower, p)
	case "regex":
		re := compileCachedRegex(pattern)
		return re != nil && re.MatchString(signatureLower)
	case "hash":
		return false
	case "string-literal":
		// Клиент (Java) сам ищет литерал в байткоде и шлёт совпавший паттерн как signature;
		// сервер лишь подтверждает точным равенством и резолвит severity.
		return signatureLower == p
	default: // substring (вкл. пустой MatchType — обратная совместимость)
		return strings.Contains(signatureLower, p)
	}
}

// matchesWord — true, если patternLower встречается в signatureLower как отдельное
// слово (границы — начало/конец строки или не-словесный символ). Ловит "java" в
// "run java now", но не в "javaw"/"myjava" — резко снижает ложные срабатывания.
func matchesWord(signatureLower, patternLower string) bool {
	from := 0
	for {
		i := strings.Index(signatureLower[from:], patternLower)
		if i < 0 {
			return false
		}
		start := from + i
		end := start + len(patternLower)
		leftOk := start == 0 || !isWordChar(signatureLower[start-1])
		rightOk := end == len(signatureLower) || !isWordChar(signatureLower[end])
		if leftOk && rightOk {
			return true
		}
		from = start + 1
		if from >= len(signatureLower) {
			return false
		}
	}
}

func isWordChar(b byte) bool {
	return (b >= 'a' && b <= 'z') || (b >= 'A' && b <= 'Z') || (b >= '0' && b <= '9') || b == '_'
}

var (
	regexCacheMu sync.Mutex
	regexCache   = map[string]*regexp.Regexp{}
)

// compileCachedRegex компилирует regex-паттерн сигнатуры с кэшем. Go regexp — RE2, без
// catastrophic backtracking (ReDoS невозможен). Невалидный паттерн → nil (кэшируется,
// детект просто не сматчится — не паникуем).
func compileCachedRegex(pattern string) *regexp.Regexp {
	regexCacheMu.Lock()
	defer regexCacheMu.Unlock()
	if re, ok := regexCache[pattern]; ok {
		return re
	}
	re, err := regexp.Compile(pattern)
	if err != nil {
		regexCache[pattern] = nil
		return nil
	}
	regexCache[pattern] = re
	return re
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

// HwidComponents — раздельные солёные хеши компонентов железа (для fuzzy-матча HWID).
// Пустые поля игнорируются. Сырьё не передаётся — только хеши (приватность).
type HwidComponents struct {
	MachineID string   `json:"machineId"`
	BoardUUID string   `json:"boardUuid"`
	Macs      []string `json:"macs"`
}

// stableCount — число непустых СТАБИЛЬНЫХ компонентов (machine_id + board_uuid). MAC
// нестабилен (смена сетевой карты, VPN, виртуальные адаптеры) и в порог не входит.
func (c HwidComponents) stableCount() int {
	n := 0
	if c.MachineID != "" {
		n++
	}
	if c.BoardUUID != "" {
		n++
	}
	return n
}

// InitHandshake — совместимая обёртка без компонентов HWID (старый путь и тесты).
func (s *Service) InitHandshake(ctx context.Context, userUUID, login, hwidHash string, detections []DetectionInput) (InitResult, error) {
	return s.InitHandshakeWithComponents(ctx, userUUID, login, hwidHash, HwidComponents{}, detections)
}

// InitHandshakeWithComponents проверяет баны (точный + fuzzy по компонентам), фиксирует
// HWID и pre-launch детекты, и при успехе выдаёт launch-token + nonce. Блок = Allowed:false.
func (s *Service) InitHandshakeWithComponents(ctx context.Context, userUUID, login, hwidHash string, comps HwidComponents, detections []DetectionInput) (InitResult, error) {
	now := s.now()

	if banned, reason := s.accountBanned(ctx, userUUID, now); banned {
		return InitResult{Allowed: false, Reason: reason}, nil
	}
	if hwidHash != "" {
		if banned, reason := s.hwidBanned(ctx, hwidHash, comps, now); banned {
			return InitResult{Allowed: false, Reason: reason}, nil
		}
		if err := s.touchHwid(ctx, hwidHash, userUUID, login, comps, now); err != nil {
			return InitResult{}, err
		}
	}

	// Фиксируем pre-launch детекты от лаунчера (скан процессов/файлов).
	for _, d := range detections {
		if _, _, err := s.recordDetection(ctx, userUUID, login, hwidHash, "", d, now); err != nil {
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
// СЕРВЕРНУЮ severity и confidence детекта (используются для решения о kick). Клиентская
// severity из запроса игнорируется — её нельзя занизить.
func (s *Service) RecordDetection(ctx context.Context, claims LaunchClaims, d DetectionInput) (int, string, error) {
	return s.recordDetection(ctx, claims.UUID, claims.Login, claims.HwidHash, claims.Nonce, d, s.now())
}

func (s *Service) recordDetection(ctx context.Context, userUUID, login, hwidHash, sessionID string, d DetectionInput, now time.Time) (int, string, error) {
	severity, matchType := s.resolveDetection(ctx, d.Type, d.Signature, detectionHash(d.Details))
	confidence := detectionConfidence(d.Type, matchType)
	// Дедуп: одинаковый детект в пределах окна не пишем повторно, но severity/confidence
	// всё равно возвращаем — kick-решение должно срабатывать и на спамящем агенте.
	if s.isDuplicate(userUUID, sessionID, d.Type, d.Signature, now) {
		return severity, confidence, nil
	}
	raw := ""
	if d.Details != nil {
		if b, err := json.Marshal(d.Details); err == nil {
			raw = string(b)
		}
	}
	rec := models.Detection{
		ID:         uuid.NewString(),
		UserUUID:   userUUID,
		Login:      login,
		HwidHash:   hwidHash,
		SessionID:  sessionID,
		Source:     normalizeSource(d.Source),
		Type:       d.Type,
		Signature:  d.Signature,
		Severity:   severity,
		Confidence: confidence,
		Status:     "new",
		Raw:        raw,
		CreatedAt:  now,
	}
	if err := s.db.WithContext(ctx).Create(&rec).Error; err != nil {
		return severity, confidence, err
	}
	// Авто-бан только за hard-детект: soft-эвристика (substring-матч, tamper, debugger)
	// не банит даже при включённом autoBan — она лишь попадает в review-очередь.
	autoBanned := s.autoBan && severity >= autoBanSeverity && confidence == "hard"
	if autoBanned {
		s.autoBanEscalated(ctx, userUUID, login, hwidHash, d.Signature, now)
	}
	if s.notifier != nil {
		// Алерт не должен задерживать ответ лаунчеру/агенту.
		go s.notifier.NotifyDetection(rec, autoBanned)
	}
	return severity, confidence, nil
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

func (s *Service) hwidBanned(ctx context.Context, hwidHash string, comps HwidComponents, now time.Time) (bool, string) {
	// 1. Точный агрегат — старые баны и клиенты без компонентов.
	var ban models.HwidBan
	err := s.db.WithContext(ctx).Where("hwid_hash = ?", hwidHash).First(&ban).Error
	if err == nil && (ban.ExpiresAt == nil || !now.After(*ban.ExpiresAt)) {
		return true, banReason("Устройство заблокировано", ban.Reason)
	}
	// 2. Fuzzy: смена нестабильного компонента (MAC) меняет агрегат, но не обходит бан,
	//    если совпали ОБА стабильных компонента. Нужен весомый отпечаток (≥2 стабильных) —
	//    одиночное совпадение не банит (защита от коллизии общего образа/партии машин).
	if comps.stableCount() >= 2 {
		if reason := s.fuzzyHwidBanned(ctx, comps, now); reason != "" {
			return true, reason
		}
	}
	return false, ""
}

// fuzzyHwidBanned ищет активный бан, чей HWID совпадает с текущим по ОБОИМ стабильным
// компонентам (machine_id И board_uuid). Возвращает причину или "". Один совпавший
// компонент НЕ банит — защита от коллизии (общий образ ОС, партия одинаковых машин).
func (s *Service) fuzzyHwidBanned(ctx context.Context, comps HwidComponents, now time.Time) string {
	var bans []models.HwidBan
	if err := s.db.WithContext(ctx).Find(&bans).Error; err != nil {
		return ""
	}
	for _, ban := range bans {
		if ban.ExpiresAt != nil && now.After(*ban.ExpiresAt) {
			continue
		}
		var h models.Hwid
		if err := s.db.WithContext(ctx).Where("hash = ?", ban.HwidHash).First(&h).Error; err != nil {
			continue
		}
		if comps.MachineID != "" && comps.MachineID == h.MachineIDHash &&
			comps.BoardUUID != "" && comps.BoardUUID == h.BoardUUIDHash {
			return banReason("Устройство заблокировано", ban.Reason)
		}
	}
	return ""
}

func (s *Service) touchHwid(ctx context.Context, hwidHash, userUUID, login string, comps HwidComponents, now time.Time) error {
	macJSON := ""
	if len(comps.Macs) > 0 {
		if b, err := json.Marshal(comps.Macs); err == nil {
			macJSON = string(b)
		}
	}
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
			MachineIDHash: comps.MachineID,
			BoardUUIDHash: comps.BoardUUID,
			MacHashes:     macJSON,
		}).Error
	}
	if err != nil {
		return err
	}
	updates := map[string]any{
		"seen_count": gorm.Expr("seen_count + 1"),
		"last_seen":  now,
	}
	// Компоненты обновляем только если присланы — старый клиент их не шлёт, не затираем.
	if comps.MachineID != "" {
		updates["machine_id_hash"] = comps.MachineID
	}
	if comps.BoardUUID != "" {
		updates["board_uuid_hash"] = comps.BoardUUID
	}
	if macJSON != "" {
		updates["mac_hashes"] = macJSON
	}
	return s.db.WithContext(ctx).Model(&models.Hwid{}).Where("hash = ?", hwidHash).Updates(updates).Error
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

// DetectionFilter — необязательные фильтры review-очереди (пустые поля игнорируются).
type DetectionFilter struct {
	Status      string // new|reviewed|confirmed|dismissed
	Confidence  string // hard|soft
	MinSeverity int
}

func (s *Service) ListDetections(ctx context.Context, limit int, filter DetectionFilter) ([]models.Detection, error) {
	if limit <= 0 || limit > 500 {
		limit = 100
	}
	q := s.db.WithContext(ctx).Order("created_at desc").Limit(limit)
	if filter.Status != "" {
		q = q.Where("status = ?", filter.Status)
	}
	if filter.Confidence != "" {
		q = q.Where("confidence = ?", filter.Confidence)
	}
	if filter.MinSeverity > 0 {
		q = q.Where("severity >= ?", filter.MinSeverity)
	}
	var out []models.Detection
	err := q.Find(&out).Error
	return out, err
}

// validDetectionStatuses — допустимые значения статуса review-очереди.
var validDetectionStatuses = map[string]bool{
	"new": true, "reviewed": true, "confirmed": true, "dismissed": true,
}

// UpdateDetectionStatus меняет статус разбора детекта оператором и фиксирует, кто и
// когда его проставил. Невалидный статус отклоняется.
func (s *Service) UpdateDetectionStatus(ctx context.Context, id, status, adminLogin string) error {
	if !validDetectionStatuses[status] {
		return errors.New("invalid detection status")
	}
	now := s.now()
	return s.db.WithContext(ctx).Model(&models.Detection{}).Where("id = ?", id).Updates(map[string]any{
		"status":      status,
		"reviewed_by": adminLogin,
		"reviewed_at": now,
	}).Error
}

// SignatureStat — агрегат детектов по одной сигнатуре (shadow-телеметрия, оценка FP).
type SignatureStat struct {
	Signature     string `json:"signature"`
	Type          string `json:"type"`
	Confidence    string `json:"confidence"`
	Total         int64  `json:"total"`         // всего срабатываний
	UniquePlayers int64  `json:"uniquePlayers"` // уникальных игроков (distinct user_uuid)
	NewCount      int64  `json:"new"`           // не разобрано оператором
	Confirmed     int64  `json:"confirmed"`     // подтверждено (истинный детект)
	Dismissed     int64  `json:"dismissed"`     // отклонено (ложняк)
}

// SignatureStats агрегирует детекты с момента since по (signature, type, confidence):
// число срабатываний, уникальных игроков и разбивку по статусу review. Сигнатура с
// большим total на многих игроках при нуле confirmed — кандидат в ложняки: её надо
// сузить (match_type) или отключить, а не банить. Главный инструмент перед autoBan.
func (s *Service) SignatureStats(ctx context.Context, since time.Time) ([]SignatureStat, error) {
	var out []SignatureStat
	err := s.db.WithContext(ctx).
		Model(&models.Detection{}).
		Select(`signature,
			type,
			confidence,
			COUNT(*) AS total,
			COUNT(DISTINCT user_uuid) AS unique_players,
			SUM(CASE WHEN status = 'confirmed' THEN 1 ELSE 0 END) AS confirmed,
			SUM(CASE WHEN status = 'dismissed' THEN 1 ELSE 0 END) AS dismissed,
			SUM(CASE WHEN status = 'new' THEN 1 ELSE 0 END) AS new_count`).
		Where("created_at >= ?", since).
		Group("signature, type, confidence").
		Order("total DESC").
		Scan(&out).Error
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

const maxPatternLen = 255

// validateSignature проверяет сигнатуру перед сохранением: лимит длины Pattern и
// компилируемость regex (для MatchType "regex"). Невалидный regex отклоняется здесь,
// а не молча игнорируется в рантайме матчинга.
func validateSignature(matchType, pattern string) error {
	if len(pattern) > maxPatternLen {
		return errors.New("pattern too long")
	}
	if matchType == "regex" {
		if _, err := regexp.Compile(pattern); err != nil {
			return errors.New("invalid regex: " + err.Error())
		}
	}
	return nil
}

func (s *Service) CreateSignature(ctx context.Context, sig models.CheatSignature) (models.CheatSignature, error) {
	if sig.MatchType == "" {
		sig.MatchType = "substring"
	}
	if err := validateSignature(sig.MatchType, sig.Pattern); err != nil {
		return models.CheatSignature{}, err
	}
	sig.ID = uuid.NewString()
	now := s.now()
	sig.CreatedAt = now
	sig.UpdatedAt = now
	err := s.db.WithContext(ctx).Create(&sig).Error
	return sig, err
}

func (s *Service) UpdateSignature(ctx context.Context, id string, updates map[string]any) error {
	// Валидируем эффективные match_type/pattern (с учётом частичного апдейта): читаем
	// существующую запись, накладываем изменения и проверяем regex-компиляцию/длину.
	var existing models.CheatSignature
	if err := s.db.WithContext(ctx).Where("id = ?", id).First(&existing).Error; err != nil {
		return err
	}
	mt := effectiveMatchType(existing.MatchType)
	pat := existing.Pattern
	if v, ok := updates["match_type"].(string); ok {
		mt = v
	}
	if v, ok := updates["pattern"].(string); ok {
		pat = v
	}
	if err := validateSignature(mt, pat); err != nil {
		return err
	}
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
