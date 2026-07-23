package yggdrasil

import (
	"context"
	"crypto/md5"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"math/big"
	"net/url"
	"strings"

	"launcher-backend/internal/models"

	"gorm.io/gorm"
)

// Service реализует Yggdrasil-логику: выпуск игровых сессий, join/hasJoined,
// построение профилей. UUID игрока берётся из provider UUID (нормализованный).
type Service struct {
	db           *gorm.DB
	store        *Store
	keys         *KeyPair
	baseURL      string
	serverName   string
	injectorPath string
}

func NewService(db *gorm.DB, keys *KeyPair, baseURL, serverName, injectorPath string) *Service {
	return &Service{
		db:           db,
		store:        NewStore(db),
		keys:         keys,
		baseURL:      strings.TrimRight(baseURL, "/"),
		serverName:   serverName,
		injectorPath: injectorPath,
	}
}

// InjectorPath — путь к authlib-injector.jar на диске (отдаётся лаунчеру).
func (s *Service) InjectorPath() string { return s.injectorPath }

// Profile — ответ Yggdrasil с профилем игрока.
type Profile struct {
	ID         string     `json:"id"`
	Name       string     `json:"name"`
	Properties []Property `json:"properties"`
}

type Property struct {
	Name      string `json:"name"`
	Value     string `json:"value"`
	Signature string `json:"signature,omitempty"`
}

// IssueSession выпускает новую игровую сессию для пользователя лаунчера. nonce
// связывает её с launch-token античита (handshake/init); пустой nonce допустим
// (сессия не пройдёт verified-гейт на join без последующего confirm).
func (s *Service) IssueSession(user models.User, nonce string) Session {
	sess := Session{
		AccessToken: randomToken(),
		ClientToken: randomToken(),
		UUID:        NormalizeUUID(user.ProviderUUID, user.Login),
		Name:        user.Login,
		Nonce:       nonce,
	}
	s.store.PutSession(sess)
	return sess
}

func (s *Service) Store() *Store { return s.store }

// profileFor собирает Yggdrasil-профиль. Скины пока не отдаём — клиент покажет
// дефолтный Steve/Alex, enforcement от этого не зависит.
func (s *Service) profileFor(uuid, name string) Profile {
	return Profile{
		ID:         uuid,
		Name:       name,
		Properties: []Property{},
	}
}

// LookupByNames возвращает профили (id+name) для запрошенных ников — для
// api/profiles/minecraft, которым сервер резолвит whitelist/ops.
func (s *Service) LookupByNames(ctx context.Context, names []string) []Profile {
	if len(names) == 0 {
		return nil
	}
	var users []models.User
	if err := s.db.WithContext(ctx).Where("login IN ?", names).Find(&users).Error; err != nil {
		return nil
	}
	profiles := make([]Profile, 0, len(users))
	for _, user := range users {
		profiles = append(profiles, s.profileFor(NormalizeUUID(user.ProviderUUID, user.Login), user.Login))
	}
	return profiles
}

// LookupByUUID находит игрока по нормализованному UUID (для profile/{uuid}).
func (s *Service) LookupByUUID(ctx context.Context, uuid string) (Profile, bool) {
	clean := normalizeHex(uuid)
	if len(clean) != 32 {
		return Profile{}, false
	}
	// Индексный поиск вместо полного скана users (раньше — CPU/DB-DoS: анонимный
	// /profile/:uuid грузил и линейно сканировал всю таблицу на каждый хит). И
	// provider_uuid (uniqueIndex), и id (PK) проиндексированы. Зарегистрированные
	// игроки хранят UUID с дефисами (OfflinePlayerUUIDString), поэтому ищем обе формы;
	// NormalizeUUID подтверждает совпадение.
	// ponytail: не покрывает http-провайдерных юзеров с НЕ-hex provider_uuid (косметика
	// скина); AUTH_MODE=local (прод/дефолт) хранит hex — им это не грозит.
	dashed := clean[0:8] + "-" + clean[8:12] + "-" + clean[12:16] + "-" + clean[16:20] + "-" + clean[20:32]
	forms := []string{clean, dashed}
	var users []models.User
	if err := s.db.WithContext(ctx).
		Where("provider_uuid IN ? OR id IN ?", forms, forms).
		Limit(8).Find(&users).Error; err != nil {
		return Profile{}, false
	}
	for _, user := range users {
		if NormalizeUUID(user.ProviderUUID, user.Login) == clean {
			return s.profileFor(clean, user.Login), true
		}
	}
	return Profile{}, false
}

// Meta — корневой ответ authlib-injector (URL, переданный в javaagent).
func (s *Service) Meta() map[string]any {
	skinDomain := ""
	if parsed, err := url.Parse(s.baseURL); err == nil {
		skinDomain = parsed.Hostname()
	}
	skinDomains := []string{}
	if skinDomain != "" {
		skinDomains = []string{skinDomain}
	}
	return map[string]any{
		"meta": map[string]any{
			"serverName":              s.serverName,
			"implementationName":      "launcher-backend-yggdrasil",
			"implementationVersion":   "1.0.0",
			"feature.non_email_login": true,
		},
		"skinDomains":        skinDomains,
		"signaturePublickey": s.keys.PublicKeyPEM(),
	}
}

// NormalizeUUID приводит provider UUID к 32-hex без дефисов. Если значение не
// похоже на UUID — генерирует offline-UUID из ника (как offline-mode сервер).
func NormalizeUUID(raw, name string) string {
	clean := normalizeHex(raw)
	if len(clean) == 32 {
		return clean
	}
	return OfflineUUID(name)
}

// canonicalServerID приводит Minecraft serverId-хеш к каноничному виду:
// беззнаковые 20 байт SHA-1 → 40 hex-символов в нижнем регистре. Разные клиенты
// кодируют один и тот же хеш по-разному (Java authlib — знаковый BigInteger в
// нижнем регистре, напр. "-25e3..."; прокси Void — беззнаковый в верхнем регистре,
// напр. "70B0..."). Канонизация в /join и /hasJoined убирает расхождения по
// регистру, знаку и ведущим нулям, иначе map-lookup в Store промахивается.
func canonicalServerID(raw string) string {
	s := strings.TrimSpace(raw)
	neg := strings.HasPrefix(s, "-")
	if neg {
		s = s[1:]
	}
	n, ok := new(big.Int).SetString(s, 16)
	if !ok {
		return strings.ToLower(strings.TrimSpace(raw))
	}
	if neg {
		mod := new(big.Int).Lsh(big.NewInt(1), 160)
		n.Mod(n.Sub(mod, n), mod)
	}
	return fmt.Sprintf("%040x", n)
}

func normalizeHex(raw string) string {
	clean := strings.ToLower(strings.ReplaceAll(strings.TrimSpace(raw), "-", ""))
	for _, r := range clean {
		if !((r >= '0' && r <= '9') || (r >= 'a' && r <= 'f')) {
			return ""
		}
	}
	return clean
}

// OfflineUUID повторяет схему Minecraft offline-mode: MD5("OfflinePlayer:"+name)
// с проставленными битами версии 3 и варианта.
func OfflineUUID(name string) string {
	sum := md5.Sum([]byte("OfflinePlayer:" + name))
	sum[6] = (sum[6] & 0x0f) | 0x30
	sum[8] = (sum[8] & 0x3f) | 0x80
	return hex.EncodeToString(sum[:])
}

func randomToken() string {
	buf := make([]byte, 16)
	if _, err := rand.Read(buf); err != nil {
		// rand.Read практически не падает; на всякий случай — детерминированный fallback.
		return hex.EncodeToString(md5sum(buf))
	}
	return hex.EncodeToString(buf)
}

func md5sum(data []byte) []byte {
	sum := md5.Sum(data)
	return sum[:]
}
