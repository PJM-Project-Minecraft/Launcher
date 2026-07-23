package anticheat

import (
	"strings"
	"testing"

	"launcher-backend/internal/yggdrasil"
)

type fakeSessions struct {
	byName map[string]yggdrasil.Session
}

func (f fakeSessions) ActiveSessions() []yggdrasil.OnlineSession       { return nil }
func (f fakeSessions) SessionByNonce(string) (yggdrasil.Session, bool) { return yggdrasil.Session{}, false }
func (f fakeSessions) VerifiedSessionByName(name string) (yggdrasil.Session, bool) {
	s, ok := f.byName[name]
	return s, ok
}

func TestP5Check(t *testing.T) {
	sess := yggdrasil.Session{AccessToken: "secret-access-token", Name: "Liko", Verified: true}
	h := Handler{sessions: fakeSessions{byName: map[string]yggdrasil.Session{"Liko": sess}}}

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
}
