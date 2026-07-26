package anticheat

import (
	"context"
	"testing"
	"time"
)

// Кик исполняет игровой сервер, а не агент в JVM игрока: молчание heartbeat и
// detect-kick обязаны попадать в список отзывов, а новый запуск игры (Confirm) —
// его снимать. Иначе игрок, прибивший агента, продолжает играть (было до P-1).
func TestRevokeOnAgentSilentAndDetectKick(t *testing.T) {
	now := time.Now()
	ygg := &fakeVerifier{verified: map[string]bool{}, launcherSeen: map[string]time.Time{}}
	svc := NewService(newTestDB(t), "secret", false, ygg, "")
	svc.now = func() time.Time { return now }

	res, err := svc.InitHandshake(context.Background(), "uuid-rev", "Liko", "", nil)
	if err != nil || !res.Allowed {
		t.Fatalf("init: %v %+v", err, res)
	}
	claims, err := svc.VerifyToken(res.LaunchToken)
	if err != nil {
		t.Fatalf("verify: %v", err)
	}
	// Игра запущена: сессия Verified, лаунчер шлёт keepalive, агент — heartbeat.
	ygg.MarkVerifiedByNonce(claims.Nonce)
	svc.touchHeartbeat(claims.Nonce, claims.Login)

	// Пока агент пингует — кикать некого.
	if orders := svc.RevokedAmong([]string{"Liko"}); len(orders) != 0 {
		t.Fatalf("живой агент не должен давать отзыв: %+v", orders)
	}

	// Агент замолчал дольше таймаута, а лаунчер жив → его прибили в живой игре.
	now = now.Add(5 * time.Minute)
	ygg.launcherSeen[claims.Nonce] = now
	svc.reapStale(now)
	orders := svc.RevokedAmong([]string{"Liko", "ДругойИгрок"})
	if len(orders) != 1 || orders[0].Player != "Liko" {
		t.Fatalf("ожидался отзыв только для Liko, получено %+v", orders)
	}

	// Новый запуск игры снимает отзыв.
	svc.clearRevocation("liko")
	if orders := svc.RevokedAmong([]string{"Liko"}); len(orders) != 0 {
		t.Fatalf("после Confirm отзыв должен сниматься: %+v", orders)
	}

	// detect-kick тоже отзывает доступ (сессию гасим, но игрок уже на сервере).
	if kick, _ := svc.EvaluateKick(claims, 9, "hard", "inject"); !kick {
		t.Fatal("inject должен кикать")
	}
	if orders := svc.RevokedAmong([]string{"Liko"}); len(orders) != 1 {
		t.Fatalf("detect-kick обязан отзывать доступ: %+v", orders)
	}

	// Отзыв не тянется в следующий сеанс: протух по TTL.
	now = now.Add(revokeTTL + time.Minute)
	if orders := svc.RevokedAmong([]string{"Liko"}); len(orders) != 0 {
		t.Fatalf("отзыв должен протухать по TTL: %+v", orders)
	}
}
