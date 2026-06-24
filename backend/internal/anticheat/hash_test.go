package anticheat

import (
	"context"
	"testing"

	"launcher-backend/internal/models"
)

// Hash-матч байткода: severity и match_type берутся из hash-сигнатуры по HashHex,
// независимо от имени класса (бьёт обфускацию имён — главная цель Фазы 2).
func TestHashMatch(t *testing.T) {
	svc := NewService(isolatedSigDB(t, "hash_match"), "secret", false, nil, "")
	ctx := context.Background()
	if _, err := svc.CreateSignature(ctx, models.CheatSignature{Kind: "class", MatchType: "hash", HashHex: "deadbeef", Severity: 9, Enabled: true}); err != nil {
		t.Fatalf("create: %v", err)
	}
	sev, mt := svc.resolveDetection(ctx, "class", "obfuscated.a", "deadbeef")
	if sev != 9 || mt != "hash" {
		t.Fatalf("hash-матч: ожидалось severity 9 / match_type hash, получено sev=%d mt=%q", sev, mt)
	}
	// Неверный hash не матчит — severity падает на дефолт.
	if sev, _ := svc.resolveDetection(ctx, "class", "obfuscated.a", "00000000"); sev == 9 {
		t.Fatal("неверный hash не должен давать severity сигнатуры")
	}
}

// recordDetection извлекает hash из Details и резолвит hash-детект как hard.
func TestDetectionHashFromDetails(t *testing.T) {
	db := newTestDB(t)
	svc := NewService(db, "secret", false, nil, "")
	ctx := context.Background()
	if _, err := svc.CreateSignature(ctx, models.CheatSignature{Kind: "class", MatchType: "hash", HashHex: "cafef00d", Severity: 9, Enabled: true}); err != nil {
		t.Fatalf("create: %v", err)
	}
	res, _ := svc.InitHandshake(ctx, "uuid-dh", "L", "hwid-dh", nil)
	claims, _ := svc.VerifyToken(res.LaunchToken)
	sev, conf, err := svc.RecordDetection(ctx, claims, DetectionInput{
		Type: "class", Signature: "obf.a", Details: map[string]any{"hash": "cafef00d"},
	})
	if err != nil {
		t.Fatalf("detect: %v", err)
	}
	if sev != 9 || conf != "hard" {
		t.Fatalf("hash-детект из Details: ожидалось severity 9 / hard, получено sev=%d conf=%q", sev, conf)
	}
}
