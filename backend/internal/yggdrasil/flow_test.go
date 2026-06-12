package yggdrasil

import (
	"testing"

	"launcher-backend/internal/models"
)

func newTestService() *Service {
	keys, _ := LoadOrCreateKey("/tmp/ygg_test_key.pem")
	return NewService(nil, keys, "https://example.com", "Test", "")
}

func TestNormalizeUUID(t *testing.T) {
	got := NormalizeUUID("11111111-2222-3333-4444-555555555555", "Bob")
	if got != "11111111222233334444555555555555" {
		t.Fatalf("дефисный UUID не нормализован: %q", got)
	}
	// не-UUID → offline по нику, стабильно
	a := NormalizeUUID("", "Steve")
	b := NormalizeUUID("not-a-uuid!!", "Steve")
	if a != b || len(a) != 32 {
		t.Fatalf("offline UUID нестабилен: %q vs %q", a, b)
	}
}

func TestJoinFlowAllowsLauncherPlayer(t *testing.T) {
	svc := newTestService()
	sess := svc.IssueSession(models.User{
		Login:        "Liko",
		ProviderUUID: "11111111-2222-3333-4444-555555555555",
	}, "")

	// Клиент вызывает join валидным токеном.
	got, ok := svc.Store().Session(sess.AccessToken)
	if !ok || got.UUID != "11111111222233334444555555555555" {
		t.Fatalf("сессия не сохранена корректно: %+v ok=%v", got, ok)
	}
	svc.Store().PutJoin("server-abc", JoinRecord{UUID: sess.UUID, Name: sess.Name})

	// Сервер спрашивает hasJoined тем же serverId и ником → запись есть.
	rec, ok := svc.Store().ConsumeJoin("server-abc")
	if !ok || rec.Name != "Liko" {
		t.Fatalf("hasJoined не нашёл join из лаунчера")
	}
}

func TestJoinIsSingleUse(t *testing.T) {
	svc := newTestService()
	svc.Store().PutJoin("server-xyz", JoinRecord{UUID: "u", Name: "Liko"})

	// Первый hasJoined проходит...
	if _, ok := svc.Store().ConsumeJoin("server-xyz"); !ok {
		t.Fatal("первый hasJoined должен пройти")
	}
	// ...повтор (replay перехваченного запроса) уже нет.
	if _, ok := svc.Store().ConsumeJoin("server-xyz"); ok {
		t.Fatal("повторный hasJoined должен быть отклонён (анти-replay)")
	}
}

func TestHasJoinedRejectsNonLauncherPlayer(t *testing.T) {
	svc := newTestService()
	// Пират не вызывал join → записи по его serverId нет.
	if _, ok := svc.Store().ConsumeJoin("pirate-server-id"); ok {
		t.Fatal("для не-лаунчер игрока не должно быть join-записи")
	}
}

func TestInvalidTokenHasNoSession(t *testing.T) {
	svc := newTestService()
	if _, ok := svc.Store().Session("totally-fake-token"); ok {
		t.Fatal("несуществующий accessToken не должен валидироваться")
	}
}

func TestSessionStartsUnverified(t *testing.T) {
	svc := newTestService()
	sess := svc.IssueSession(models.User{Login: "Liko", ProviderUUID: "u"}, "nonce-1")
	got, _ := svc.Store().Session(sess.AccessToken)
	if got.Verified {
		t.Fatal("новая сессия не должна быть Verified до confirm")
	}
}

func TestMarkVerifiedByNonce(t *testing.T) {
	svc := newTestService()
	sess := svc.IssueSession(models.User{Login: "Liko", ProviderUUID: "u"}, "nonce-2")

	if !svc.Store().MarkVerifiedByNonce("nonce-2") {
		t.Fatal("confirm по корректному nonce должен пройти")
	}
	got, _ := svc.Store().Session(sess.AccessToken)
	if !got.Verified {
		t.Fatal("после confirm сессия должна быть Verified")
	}
	// Повтор не проходит: сессия уже Verified (анти-replay confirm). Nonce при этом
	// сохраняется — он нужен для серверного kick/heartbeat по той же сессии.
	if svc.Store().MarkVerifiedByNonce("nonce-2") {
		t.Fatal("повторный confirm по уже подтверждённой сессии должен быть отклонён")
	}
	// Сессия остаётся активной и гасится серверным kick по тому же nonce.
	if !svc.Store().IsActiveByNonce("nonce-2") {
		t.Fatal("после confirm сессия должна считаться активной по nonce")
	}
	if !svc.Store().InvalidateByNonce("nonce-2") {
		t.Fatal("серверный kick по nonce должен найти сессию после confirm")
	}
	if svc.Store().IsActiveByNonce("nonce-2") {
		t.Fatal("после kick сессия не должна быть активной")
	}
}

func TestMarkVerifiedUnknownNonce(t *testing.T) {
	svc := newTestService()
	if svc.Store().MarkVerifiedByNonce("never-issued") {
		t.Fatal("confirm по неизвестному nonce не должен проходить")
	}
}
