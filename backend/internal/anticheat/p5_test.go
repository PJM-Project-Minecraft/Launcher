package anticheat

import (
	"strings"
	"testing"

	"launcher-backend/internal/yggdrasil"
)

type fakeSessions struct {
	byName map[string][]yggdrasil.Session
}

func (f fakeSessions) ActiveSessions() []yggdrasil.OnlineSession       { return nil }
func (f fakeSessions) SessionByNonce(string) (yggdrasil.Session, bool) { return yggdrasil.Session{}, false }
func (f fakeSessions) VerifiedSessionsByName(name string) []yggdrasil.Session {
	return f.byName[name]
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
