package anticheat

import (
	"context"
	"testing"
	"time"
)

// Регрессия ложного кика на нестабильном интернете: при обрыве связи молчат И агент,
// И лаунчер, а keepalive лаунчера (интервал 120с, таймаут 30с) возвращается раньше
// heartbeat агента (30с, таймаут 10с). Одиночной метки keepalive после grace НЕ должно
// хватать для отзыва — иначе честного игрока кикало на каждом провале сети.
func TestNoRevokeOnFlakyNetwork(t *testing.T) {
	now := time.Now()
	ygg := &fakeVerifier{verified: map[string]bool{}, launcherSeen: map[string]time.Time{},
		launcherPrev: map[string]time.Time{}}
	svc := NewService(newTestDB(t), "secret", false, ygg, "")
	svc.now = func() time.Time { return now }

	res, _ := svc.InitHandshake(context.Background(), "uuid-flaky", "Colis", "", nil)
	claims, err := svc.VerifyToken(res.LaunchToken)
	if err != nil {
		t.Fatalf("verify: %v", err)
	}
	ygg.MarkVerifiedByNonce(claims.Nonce)
	base := now
	svc.touchHeartbeat(claims.Nonce, claims.Login)

	// Связь пропала у обоих; вернулась через 2 минуты — keepalive лаунчера успел пройти
	// (одна метка), heartbeat агента ещё в пути.
	now = base.Add(2 * time.Minute)
	ygg.launcherSeen[claims.Nonce] = now
	svc.reapStale(now)
	if orders := svc.RevokedAmong([]string{"Colis"}); len(orders) != 0 {
		t.Fatalf("одна метка keepalive после обрыва не должна давать отзыв: %+v", orders)
	}

	// Агент вернулся — молчание закончилось, дальнейшие проходы reaper молчат.
	now = base.Add(2*time.Minute + 20*time.Second)
	svc.touchHeartbeat(claims.Nonce, claims.Login)
	svc.reapStale(now.Add(time.Minute))
	if orders := svc.RevokedAmong([]string{"Colis"}); len(orders) != 0 {
		t.Fatalf("вернувшийся агент не должен отзываться: %+v", orders)
	}
}

// Отзыв за молчание обязан сниматься сам, когда агент снова на связи: иначе игрок,
// переживший обрыв сети, кикался игровым сервером ещё revokeTTL (10 минут) — при
// реконнекте снова и снова. Отзыв по ДЕТЕКТУ heartbeat снимать не должен.
func TestHeartbeatClearsSilenceRevocation(t *testing.T) {
	ygg := &fakeVerifier{verified: map[string]bool{}}
	svc := NewService(newTestDB(t), "secret", false, ygg, "")
	res, _ := svc.InitHandshake(context.Background(), "uuid-clr", "Colis", "", nil)
	claims, _ := svc.VerifyToken(res.LaunchToken)
	ygg.MarkVerifiedByNonce(claims.Nonce)

	svc.Revoke(claims.Login, reasonAgentSilent)
	if orders := svc.RevokedAmong([]string{"Colis"}); len(orders) != 1 {
		t.Fatalf("отзыв должен быть выдан: %+v", orders)
	}
	svc.Heartbeat(context.Background(), claims)
	if orders := svc.RevokedAmong([]string{"Colis"}); len(orders) != 0 {
		t.Fatalf("вернувшийся heartbeat обязан снимать отзыв за молчание: %+v", orders)
	}

	// Детект — не снимается: иначе читер отменял бы себе кик собственным heartbeat.
	svc.Revoke(claims.Login, "детект: inject")
	svc.Heartbeat(context.Background(), claims)
	if orders := svc.RevokedAmong([]string{"Colis"}); len(orders) != 1 {
		t.Fatalf("отзыв по детекту heartbeat снимать НЕ должен: %+v", orders)
	}
}

// Кик исполняет игровой сервер, а не агент в JVM игрока: молчание heartbeat и
// detect-kick обязаны попадать в список отзывов, а новый запуск игры (Confirm) —
// его снимать. Иначе игрок, прибивший агента, продолжает играть (было до P-1).
func TestRevokeOnAgentSilentAndDetectKick(t *testing.T) {
	now := time.Now()
	ygg := &fakeVerifier{verified: map[string]bool{}, launcherSeen: map[string]time.Time{},
		launcherPrev: map[string]time.Time{}}
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

	// Агент замолчал дольше таймаута, а лаунчер всё это время устойчиво пингует
	// (две метки после grace) → агента прибили в живой игре.
	base := now
	now = now.Add(5 * time.Minute)
	ygg.launcherPrev[claims.Nonce] = base.Add(3 * time.Minute)
	ygg.launcherSeen[claims.Nonce] = now
	svc.reapStale(now)
	orders := svc.RevokedAmong([]string{"Liko", "ДругойИгрок"})
	if len(orders) != 1 || orders[0].Player != "Liko" {
		t.Fatalf("ожидался отзыв только для Liko, получено %+v", orders)
	}

	// Регистр в ключе отзыва значим: ник-двойник не должен ни снимать, ни ловить отзыв.
	svc.clearRevocation("liko")
	if orders := svc.RevokedAmong([]string{"liko"}); len(orders) != 0 {
		t.Fatal("отзыв для Liko не должен распространяться на другого игрока liko")
	}
	if orders := svc.RevokedAmong([]string{"Liko"}); len(orders) != 1 {
		t.Fatalf("ник-двойник не должен снимать отзыв: %+v", orders)
	}

	// Новый запуск игры снимает отзыв (Confirm передаёт claims.Login как есть).
	svc.clearRevocation(claims.Login)
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
