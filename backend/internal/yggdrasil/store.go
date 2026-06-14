package yggdrasil

import (
	"log/slog"
	"sync"
	"time"

	"gorm.io/gorm"

	"launcher-backend/internal/models"
)

const (
	sessionTTL = 15 * time.Minute
	joinTTL    = 60 * time.Second
)

// Session — выданная лаунчеру игровая сессия (Minecraft accessToken).
//
// Verified выставляется только после античит-handshake (confirm от агентов). join
// пускает лишь Verified-сессии — это рычаг принуждения: без агентов сервер не пустит
// игрока. Nonce связывает сессию с launch-token, выданным на handshake/init.
type Session struct {
	AccessToken string
	ClientToken string
	UUID        string
	Name        string
	Nonce       string
	Verified    bool
	expiresAt   time.Time
}

// JoinRecord — факт того, что клиент вызвал /join с валидным accessToken.
// На него опирается hasJoined: нет записи — игрок не из лаунчера, доступ закрыт.
type JoinRecord struct {
	UUID      string
	Name      string
	IP        string
	expiresAt time.Time
}

// Store держит активные сессии и join-записи в памяти с TTL. При наличии БД
// мутации дублируются write-through в таблицы (best-effort), а при старте
// живые записи восстанавливаются — рестарт backend не выкидывает игроков.
type Store struct {
	mu       sync.Mutex
	db       *gorm.DB              // nil — без персиста (тесты, отдельные сценарии)
	sessions map[string]Session    // accessToken -> session
	joins    map[string]JoinRecord // serverId -> join
	nonces   map[string]string     // nonce -> accessToken (для confirm от агентов)
}

func NewStore(db *gorm.DB) *Store {
	s := &Store{
		db:       db,
		sessions: make(map[string]Session),
		joins:    make(map[string]JoinRecord),
		nonces:   make(map[string]string),
	}
	s.restore()
	go s.collectGarbage()
	return s
}

// restore загружает живые сессии и join-записи после рестарта backend.
func (s *Store) restore() {
	if s.db == nil {
		return
	}
	now := time.Now()
	var sessions []models.YggdrasilSession
	if err := s.db.Where("expires_at > ?", now).Find(&sessions).Error; err != nil {
		slog.Warn("yggdrasil: restore sessions failed", "error", err)
	}
	for _, row := range sessions {
		sess := Session{
			AccessToken: row.AccessToken, ClientToken: row.ClientToken,
			UUID: row.UUID, Name: row.Name, Nonce: row.Nonce,
			Verified: row.Verified, expiresAt: row.ExpiresAt,
		}
		s.sessions[sess.AccessToken] = sess
		if sess.Nonce != "" {
			s.nonces[sess.Nonce] = sess.AccessToken
		}
	}
	var joins []models.YggdrasilJoin
	if err := s.db.Where("expires_at > ?", now).Find(&joins).Error; err != nil {
		slog.Warn("yggdrasil: restore joins failed", "error", err)
	}
	for _, row := range joins {
		s.joins[row.ServerID] = JoinRecord{UUID: row.UUID, Name: row.Name, IP: row.IP, expiresAt: row.ExpiresAt}
	}
	if len(sessions) > 0 || len(joins) > 0 {
		slog.Info("yggdrasil: sessions restored", "sessions", len(sessions), "joins", len(joins))
	}
}

// persistSession/deleteSession/persistJoin/deleteJoin — best-effort запись в БД:
// ошибка персиста не ломает игровой путь, только логируется.
func (s *Store) persistSession(sess Session) {
	if s.db == nil {
		return
	}
	row := models.YggdrasilSession{
		AccessToken: sess.AccessToken, ClientToken: sess.ClientToken,
		UUID: sess.UUID, Name: sess.Name, Nonce: sess.Nonce,
		Verified: sess.Verified, ExpiresAt: sess.expiresAt,
	}
	if err := s.db.Save(&row).Error; err != nil {
		slog.Warn("yggdrasil: persist session failed", "error", err)
	}
}

func (s *Store) deleteSession(token string) {
	if s.db == nil {
		return
	}
	if err := s.db.Delete(&models.YggdrasilSession{}, "access_token = ?", token).Error; err != nil {
		slog.Warn("yggdrasil: delete session failed", "error", err)
	}
}

func (s *Store) persistJoin(serverID string, rec JoinRecord) {
	if s.db == nil {
		return
	}
	row := models.YggdrasilJoin{ServerID: serverID, UUID: rec.UUID, Name: rec.Name, IP: rec.IP, ExpiresAt: rec.expiresAt}
	if err := s.db.Save(&row).Error; err != nil {
		slog.Warn("yggdrasil: persist join failed", "error", err)
	}
}

func (s *Store) deleteJoin(serverID string) {
	if s.db == nil {
		return
	}
	if err := s.db.Delete(&models.YggdrasilJoin{}, "server_id = ?", serverID).Error; err != nil {
		slog.Warn("yggdrasil: delete join failed", "error", err)
	}
}

func (s *Store) PutSession(sess Session) {
	sess.expiresAt = time.Now().Add(sessionTTL)
	s.mu.Lock()
	defer s.mu.Unlock()
	s.sessions[sess.AccessToken] = sess
	if sess.Nonce != "" {
		s.nonces[sess.Nonce] = sess.AccessToken
	}
	s.persistSession(sess)
}

// MarkVerifiedByNonce помечает сессию, связанную с nonce, как прошедшую античит.
//
// Nonce НЕ удаляется при успехе: он нужен дальше для серверного kick
// (InvalidateByNonce) и проверки живости (IsActiveByNonce) по той же сессии.
// Анти-replay обеспечивается тем, что уже подтверждённая (Verified) сессия второй
// confirm не принимает. Возвращает false, если nonce неизвестен, сессия истекла
// или уже подтверждена.
func (s *Store) MarkVerifiedByNonce(nonce string) bool {
	if nonce == "" {
		return false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	token, ok := s.nonces[nonce]
	if !ok {
		return false
	}
	sess, ok := s.sessions[token]
	if !ok || time.Now().After(sess.expiresAt) {
		return false
	}
	if sess.Verified {
		return false // анти-replay: повторный confirm по уже подтверждённой сессии
	}
	sess.Verified = true
	s.sessions[token] = sess
	s.persistSession(sess)
	return true
}

// IsActiveByNonce сообщает, что сессия по nonce ещё жива и прошла античит (Verified).
// Используется heartbeat- и in-game-проверками: если сессию погасили (kick через
// InvalidateByNonce) или она истекла — вернёт false, и агент/сервер кикнут игрока.
func (s *Store) IsActiveByNonce(nonce string) bool {
	if nonce == "" {
		return false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	token, ok := s.nonces[nonce]
	if !ok {
		return false
	}
	sess, ok := s.sessions[token]
	if !ok || time.Now().After(sess.expiresAt) {
		return false
	}
	return sess.Verified
}

// TouchByNonce продлевает срок жизни игровой сессии по nonce (sliding TTL из
// heartbeat античита). Heartbeat — единственный регулярный сигнал «игра запущена»
// во время сессии, поэтому он держит токен живым: без этого 15-мин TTL истекал бы
// на лету, и реконнект/рестарт сервера ловил бы «Недействительная сессия».
// No-op для неизвестного nonce или истёкшей сессии — воскрешать её нельзя
// (kick через InvalidateByNonce должен оставаться окончательным).
func (s *Store) TouchByNonce(nonce string) bool {
	if nonce == "" {
		return false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	token, ok := s.nonces[nonce]
	if !ok {
		return false
	}
	sess, ok := s.sessions[token]
	if !ok || time.Now().After(sess.expiresAt) {
		return false
	}
	sess.expiresAt = time.Now().Add(sessionTTL)
	s.sessions[token] = sess
	s.persistSession(sess)
	return true
}

func (s *Store) Session(accessToken string) (Session, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	sess, ok := s.sessions[accessToken]
	if !ok || time.Now().After(sess.expiresAt) {
		return Session{}, false
	}
	return sess, true
}

// ReplaceToken используется при /authserver/refresh: старый accessToken
// заменяется новым, сессия сохраняется.
func (s *Store) ReplaceToken(oldToken, newToken string) (Session, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	sess, ok := s.sessions[oldToken]
	if !ok || time.Now().After(sess.expiresAt) {
		return Session{}, false
	}
	delete(s.sessions, oldToken)
	sess.AccessToken = newToken
	sess.expiresAt = time.Now().Add(sessionTTL)
	s.sessions[newToken] = sess
	// Nonce-индекс должен указывать на новый токен (Verified сохраняется в sess).
	if sess.Nonce != "" {
		s.nonces[sess.Nonce] = newToken
	}
	s.deleteSession(oldToken)
	s.persistSession(sess)
	return sess, true
}

// InvalidateByNonce гасит игровую сессию, связанную с nonce (kick от античита):
// предотвращает повторный вход с тем же токеном. Возвращает true, если сессия была.
func (s *Store) InvalidateByNonce(nonce string) bool {
	if nonce == "" {
		return false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	token, ok := s.nonces[nonce]
	if !ok {
		return false
	}
	delete(s.nonces, nonce)
	if _, ok := s.sessions[token]; ok {
		delete(s.sessions, token)
		s.deleteSession(token)
		return true
	}
	return false
}

func (s *Store) Invalidate(accessToken string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if sess, ok := s.sessions[accessToken]; ok && sess.Nonce != "" {
		delete(s.nonces, sess.Nonce)
	}
	delete(s.sessions, accessToken)
	s.deleteSession(accessToken)
}

// TouchSession продлевает срок жизни сессии (sliding TTL): пока игрок активно
// переподключается, токен жив; после выхода из игры лаунчер его гасит.
func (s *Store) TouchSession(accessToken string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if sess, ok := s.sessions[accessToken]; ok {
		sess.expiresAt = time.Now().Add(sessionTTL)
		s.sessions[accessToken] = sess
		s.persistSession(sess)
	}
}

func (s *Store) PutJoin(serverID string, record JoinRecord) {
	record.expiresAt = time.Now().Add(joinTTL)
	s.mu.Lock()
	defer s.mu.Unlock()
	s.joins[serverID] = record
	s.persistJoin(serverID, record)
}

// ConsumeJoin возвращает join-запись и сразу удаляет её — один join проверяется
// ровно один раз (анти-replay для hasJoined).
func (s *Store) ConsumeJoin(serverID string) (JoinRecord, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	record, ok := s.joins[serverID]
	delete(s.joins, serverID)
	s.deleteJoin(serverID)
	if !ok || time.Now().After(record.expiresAt) {
		return JoinRecord{}, false
	}
	return record, true
}

func (s *Store) collectGarbage() {
	for range time.Tick(time.Minute) {
		now := time.Now()
		s.mu.Lock()
		for token, sess := range s.sessions {
			if now.After(sess.expiresAt) {
				delete(s.sessions, token)
				if sess.Nonce != "" {
					delete(s.nonces, sess.Nonce)
				}
			}
		}
		for serverID, record := range s.joins {
			if now.After(record.expiresAt) {
				delete(s.joins, serverID)
			}
		}
		if s.db != nil {
			if err := s.db.Delete(&models.YggdrasilSession{}, "expires_at <= ?", now).Error; err != nil {
				slog.Warn("yggdrasil: gc sessions failed", "error", err)
			}
			if err := s.db.Delete(&models.YggdrasilJoin{}, "expires_at <= ?", now).Error; err != nil {
				slog.Warn("yggdrasil: gc joins failed", "error", err)
			}
		}
		s.mu.Unlock()
	}
}
