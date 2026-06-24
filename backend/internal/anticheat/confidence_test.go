package anticheat

import (
	"context"
	"testing"

	"launcher-backend/internal/models"
)

// detectionConfidence делит детекты на hard (высокая уверенность — кандидат на
// авто-действие) и soft (эвристика, возможен ложняк — только в review-очередь).
func TestDetectionConfidenceClassification(t *testing.T) {
	// Системная инъекция/attach — всегда hard (match_type не важен).
	for _, dt := range []string{"inject", "attach"} {
		if got := detectionConfidence(dt, ""); got != "hard" {
			t.Errorf("%q должен быть hard, получено %q", dt, got)
		}
	}
	// Стартовые эвристики — soft независимо от match_type.
	for _, dt := range []string{"tamper", "debugger", "module-unknown", "ld-preload"} {
		if got := detectionConfidence(dt, ""); got != "soft" {
			t.Errorf("%q должен быть soft, получено %q", dt, got)
		}
	}
	// Сигнатурный матч: substring → soft (возможен ложняк), точные типы → hard.
	if got := detectionConfidence("process", "substring"); got != "soft" {
		t.Errorf("substring-матч должен быть soft, получено %q", got)
	}
	for _, mt := range []string{"exact", "word", "regex", "hash"} {
		if got := detectionConfidence("process", mt); got != "hard" {
			t.Errorf("match_type %q должен давать hard, получено %q", mt, got)
		}
	}
}

// Soft-детект высокой severity НЕ должен авто-банить даже при включённом autoBan.
// Регрессия: tamper:native-agent-missing получает серверную severity 8 (>=autoBanSeverity),
// и легального игрока без загрузившейся нативки забанило бы при autoBan=true.
func TestSoftDetectionDoesNotAutoBan(t *testing.T) {
	db := newTestDB(t)
	svc := NewService(db, "secret", true, nil, "") // autoBan включён
	ctx := context.Background()

	res, _ := svc.InitHandshake(ctx, "uuid-soft", "Eve", "hwid-soft", nil)
	claims, _ := svc.VerifyToken(res.LaunchToken)
	sev, _, err := svc.RecordDetection(ctx, claims, DetectionInput{Type: "tamper", Signature: "native-agent-missing"})
	if err != nil {
		t.Fatalf("detect: %v", err)
	}
	if sev < autoBanSeverity {
		t.Fatalf("серверная severity tamper должна быть >= %d, получено %d", autoBanSeverity, sev)
	}
	var accBans, hwidBans int64
	db.Model(&models.AccountBan{}).Where("user_uuid = ?", "uuid-soft").Count(&accBans)
	db.Model(&models.HwidBan{}).Where("hwid_hash = ?", "hwid-soft").Count(&hwidBans)
	if accBans != 0 || hwidBans != 0 {
		t.Fatalf("soft-детект (tamper) не должен авто-банить при severity %d: acc=%d hwid=%d", sev, accBans, hwidBans)
	}
}

// Soft-детект НЕ кикает игрока из игры, даже если severity >= порога кика.
// Тот же баг tamper:native-agent-missing: severity 8 >= kickSeverity 7 → ложный кик.
func TestEvaluateKickSkipsSoftDetection(t *testing.T) {
	v := &fakeVerifier{verified: map[string]bool{}}
	svc := NewService(newTestDB(t), "secret", false, v, "")
	res, _ := svc.InitHandshake(context.Background(), "uuid-tk", "Liko", "hwid-tk", nil)
	if err := svc.Confirm(res.LaunchToken, ConfirmProof{}); err != nil {
		t.Fatalf("confirm: %v", err)
	}
	claims, _ := svc.VerifyToken(res.LaunchToken)

	if kick, _ := svc.EvaluateKick(claims, 8, "soft", "tamper"); kick {
		t.Fatal("soft-детект (tamper native-agent-missing) не должен кикать легального игрока")
	}
	if !v.IsActiveByNonce(res.Nonce) {
		t.Fatal("сессия не должна гаситься soft-детектом")
	}
}

// Hard-детект высокой severity по-прежнему кикает (attach=9) и inject кикает всегда.
func TestEvaluateKickHardStillKicks(t *testing.T) {
	v := &fakeVerifier{verified: map[string]bool{}}
	svc := NewService(newTestDB(t), "secret", false, v, "")
	res, _ := svc.InitHandshake(context.Background(), "uuid-hk2", "Liko", "hwid-hk2", nil)
	if err := svc.Confirm(res.LaunchToken, ConfirmProof{}); err != nil {
		t.Fatalf("confirm: %v", err)
	}
	claims, _ := svc.VerifyToken(res.LaunchToken)
	if kick, _ := svc.EvaluateKick(claims, 9, "hard", "attach"); !kick {
		t.Fatal("hard-детект attach severity 9 обязан кикать")
	}
}
