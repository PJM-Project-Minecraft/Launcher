package anticheat

import (
	"strings"
	"testing"
	"time"

	"launcher-backend/internal/yggdrasil"
)

type fakeSessions struct {
	byName  map[string][]yggdrasil.Session
	byNonce map[string]yggdrasil.Session
}

func (f fakeSessions) ActiveSessions() []yggdrasil.OnlineSession { return nil }
func (f fakeSessions) SessionByNonce(nonce string) (yggdrasil.Session, bool) {
	sess, ok := f.byNonce[nonce]
	return sess, ok
}
func (f fakeSessions) VerifiedSessionsByName(name string) []yggdrasil.Session {
	return f.byName[name]
}

func TestP5LeaseBindsNonceAndServerIdentity(t *testing.T) {
	sess := yggdrasil.Session{UUID: "11111111222233334444555555555555", Name: "Liko", Nonce: "nonce-live", Verified: true}
	now := time.Unix(1_700_000_000, 0)
	svc := NewService(newTestDB(t), "secret", false, nil, "")
	svc.now = func() time.Time { return now }
	svc.touchHeartbeat(sess.Nonce, sess.Name)
	h := Handler{service: svc, sessions: fakeSessions{byNonce: map[string]yggdrasil.Session{"nonce-live": sess}}}

	valid := h.p5LeaseStatus(p5LeaseConnection{PlayerUUID: sess.UUID, PlayerName: "liko", Nonce: sess.Nonce})
	if !valid.Valid || valid.LeaseRemainingMillis != 180_000 {
		t.Fatalf("live matching session did not renew lease: %+v", valid)
	}
	now = now.Add(30 * time.Second)
	decreasing := h.p5LeaseStatus(p5LeaseConnection{PlayerUUID: sess.UUID, PlayerName: sess.Name, Nonce: sess.Nonce})
	if !decreasing.Valid || decreasing.LeaseRemainingMillis != 150_000 {
		t.Fatalf("lease poll extended reporter deadline instead of decreasing it: %+v", decreasing)
	}
	for name, connection := range map[string]p5LeaseConnection{
		"borrowed nonce": {PlayerUUID: "victim-uuid", PlayerName: "Victim", Nonce: sess.Nonce},
		"missing nonce":  {PlayerUUID: sess.UUID, PlayerName: sess.Name, Nonce: "missing"},
	} {
		t.Run(name, func(t *testing.T) {
			if got := h.p5LeaseStatus(connection); got.Valid {
				t.Fatalf("invalid connection renewed lease: %+v", got)
			}
		})
	}
	now = now.Add(151 * time.Second)
	if got := h.p5LeaseStatus(p5LeaseConnection{PlayerUUID: sess.UUID, PlayerName: sess.Name, Nonce: sess.Nonce}); got.Valid || got.Reason != "reporters_stale" {
		t.Fatalf("stale reporter renewed lease: %+v", got)
	}
}

func TestP5Check(t *testing.T) {
	sess := yggdrasil.Session{AccessToken: "secret-access-token", Name: "Liko", Verified: true}
	h := Handler{sessions: fakeSessions{byName: map[string][]yggdrasil.Session{"Liko": {sess}}}}

	challenge := "chal-123"
	good := p5Proof(challenge, sess.AccessToken)

	if reason, ok := h.p5Check("Liko", challenge, good); !ok {
		t.Fatalf("валидный proof должен проходить, reason=%q", reason)
	}
	if _, ok := h.p5Check("Liko", challenge, "deadbeef"); ok {
		t.Fatal("неверный proof должен отклоняться")
	}
	if reason, ok := h.p5Check("Unknown", challenge, good); ok || reason != "no_verified_session" {
		t.Fatalf("без сессии — отказ, got ok=%v reason=%q", ok, reason)
	}
	// proof регистронезависим и с обрезкой пробелов.
	if _, ok := h.p5Check("Liko", challenge, " "+strings.ToUpper(good)+" "); !ok {
		t.Fatal("proof должен быть case-insensitive и trimmed")
	}
	// Другой challenge → другой proof → отказ (proof привязан к челленджу).
	if _, ok := h.p5Check("Liko", "other-chal", good); ok {
		t.Fatal("proof для другого challenge должен отклоняться")
	}
	// Пустой proof (клиент не ответил на challenge) — отдельная причина, не bad_proof.
	if reason, ok := h.p5Check("Liko", challenge, "  "); ok || reason != "no_response" {
		t.Fatalf("пустой proof → no_response, got ok=%v reason=%q", ok, reason)
	}
}

// После обрыва у игрока какое-то время живут ДВЕ Verified-сессии (старая доживает TTL).
// Клиент считает proof на своём токене — подойти должна любая из них, иначе честный
// игрок ловил кик по монетке (какую сессию вернёт итерация по map).
func TestP5CheckMatchesAnyLiveSession(t *testing.T) {
	stale := yggdrasil.Session{AccessToken: "stale-token", Name: "Liko", Verified: true}
	fresh := yggdrasil.Session{AccessToken: "fresh-token", Name: "Liko", Verified: true}
	h := Handler{sessions: fakeSessions{byName: map[string][]yggdrasil.Session{"Liko": {stale, fresh}}}}

	challenge := "chal-123"
	for _, tok := range []string{stale.AccessToken, fresh.AccessToken} {
		if reason, ok := h.p5Check("Liko", challenge, p5Proof(challenge, tok)); !ok {
			t.Fatalf("proof на токене %q должен проходить, reason=%q", tok, reason)
		}
	}
	if reason, ok := h.p5Check("Liko", challenge, p5Proof(challenge, "someone-else")); ok || reason != "bad_proof" {
		t.Fatalf("чужой токен — отказ, got ok=%v reason=%q", ok, reason)
	}
}
