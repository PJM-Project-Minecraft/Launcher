package anticheat

import (
	"testing"
	"time"
)

func TestTokenRoundTrip(t *testing.T) {
	signer := NewTokenSigner("secret-key")
	now := time.Unix(1_700_000_000, 0)
	claims := LaunchClaims{
		UUID:     "11111111222233334444555555555555",
		Login:    "Liko",
		HwidHash: "abc",
		Nonce:    "n0nce",
		IssuedAt: now.Unix(),
		Expires:  now.Add(120 * time.Second).Unix(),
	}
	token, err := signer.Sign(claims)
	if err != nil {
		t.Fatalf("sign: %v", err)
	}
	got, err := signer.Verify(token, now.Add(10*time.Second))
	if err != nil {
		t.Fatalf("verify: %v", err)
	}
	if got.UUID != claims.UUID || got.Nonce != claims.Nonce || got.Login != claims.Login {
		t.Fatalf("claims не совпали: %+v", got)
	}
}

func TestTokenExpired(t *testing.T) {
	signer := NewTokenSigner("secret-key")
	now := time.Unix(1_700_000_000, 0)
	token, _ := signer.Sign(LaunchClaims{UUID: "u", Nonce: "n1", Expires: now.Add(120 * time.Second).Unix()})
	claims, err := signer.Verify(token, now.Add(200*time.Second))
	if err != ErrTokenExpired {
		t.Fatalf("ожидалось ErrTokenExpired, получено %v", err)
	}
	// Просроченный (но корректно подписанный) токен обязан возвращать claims:
	// VerifySessionToken по claims.Nonce решает, жива ли ещё игровая сессия.
	if claims.UUID != "u" || claims.Nonce != "n1" {
		t.Fatalf("при ErrTokenExpired claims должны быть заполнены, получено %+v", claims)
	}
}

func TestTokenWrongSecretRejected(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	token, _ := NewTokenSigner("real-secret").Sign(LaunchClaims{UUID: "u", Expires: now.Add(120 * time.Second).Unix()})
	if _, err := NewTokenSigner("attacker-secret").Verify(token, now); err != ErrTokenSignature {
		t.Fatalf("ожидалось ErrTokenSignature, получено %v", err)
	}
}

func TestTokenTamperedPayloadRejected(t *testing.T) {
	signer := NewTokenSigner("secret-key")
	now := time.Unix(1_700_000_000, 0)
	token, _ := signer.Sign(LaunchClaims{UUID: "u", Expires: now.Add(120 * time.Second).Unix()})
	// Меняем один символ payload — подпись больше не сойдётся.
	tampered := "X" + token[1:]
	if _, err := signer.Verify(tampered, now); err == nil {
		t.Fatal("подделанный токен не должен проходить проверку")
	}
}

func TestTokenMalformed(t *testing.T) {
	signer := NewTokenSigner("secret-key")
	if _, err := signer.Verify("no-dot-here", time.Now()); err != ErrTokenMalformed {
		t.Fatalf("ожидалось ErrTokenMalformed, получено %v", err)
	}
}
