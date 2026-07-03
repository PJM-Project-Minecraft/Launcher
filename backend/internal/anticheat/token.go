package anticheat

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"strings"
	"time"
)

// LaunchToken — короткоживущий подписанный токен, выдаётся лаунчеру на /handshake/init
// и предъявляется агентами на /handshake/confirm и при репортах. Подписан HMAC-SHA256
// серверным секретом — валидирует только backend, агенты лишь пересылают значение.
type LaunchClaims struct {
	UUID      string `json:"uuid"`
	Login     string `json:"login"`
	HwidHash  string `json:"hwid"`
	Nonce     string `json:"nonce"`
	Challenge string `json:"chal"` // attestation: агент обязан вернуть его в confirm-proof
	IssuedAt  int64  `json:"iat"`
	Expires   int64  `json:"exp"`
}

var (
	ErrTokenMalformed = errors.New("malformed launch token")
	ErrTokenSignature = errors.New("invalid launch token signature")
	ErrTokenExpired   = errors.New("launch token expired")
)

var b64 = base64.RawURLEncoding

// TokenSigner подписывает и проверяет launch-token на заданном секрете.
type TokenSigner struct {
	secret []byte
}

func NewTokenSigner(secret string) TokenSigner {
	return TokenSigner{secret: []byte(secret)}
}

// Sign сериализует claims и возвращает строку вида base64(payload).base64(mac).
func (s TokenSigner) Sign(claims LaunchClaims) (string, error) {
	payload, err := json.Marshal(claims)
	if err != nil {
		return "", err
	}
	encPayload := b64.EncodeToString(payload)
	mac := s.mac(encPayload)
	return encPayload + "." + b64.EncodeToString(mac), nil
}

// Verify проверяет подпись и срок действия, возвращает claims.
func (s TokenSigner) Verify(token string, now time.Time) (LaunchClaims, error) {
	encPayload, encMac, ok := strings.Cut(token, ".")
	if !ok || encPayload == "" || encMac == "" {
		return LaunchClaims{}, ErrTokenMalformed
	}
	gotMac, err := b64.DecodeString(encMac)
	if err != nil {
		return LaunchClaims{}, ErrTokenMalformed
	}
	if !hmac.Equal(gotMac, s.mac(encPayload)) {
		return LaunchClaims{}, ErrTokenSignature
	}
	payload, err := b64.DecodeString(encPayload)
	if err != nil {
		return LaunchClaims{}, ErrTokenMalformed
	}
	var claims LaunchClaims
	if err := json.Unmarshal(payload, &claims); err != nil {
		return LaunchClaims{}, ErrTokenMalformed
	}
	if now.Unix() > claims.Expires {
		// Подпись сошлась — claims подлинные, просто старые. Возвращаем их вместе
		// с ошибкой: VerifySessionToken по claims.Nonce проверяет, жива ли ещё
		// игровая сессия (in-game-эндпоинты работают дольше 120с TTL токена).
		return claims, ErrTokenExpired
	}
	return claims, nil
}

func (s TokenSigner) mac(encPayload string) []byte {
	h := hmac.New(sha256.New, s.secret)
	h.Write([]byte(encPayload))
	return h.Sum(nil)
}
