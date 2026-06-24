package anticheat

import (
	"context"
	"testing"

	"launcher-backend/internal/models"
)

// string-literal — клиент (Java) находит сигнатурную строку в constant pool класса и
// шлёт её как signature; сервер exact-сверяет с сигнатурой и резолвит severity (hard).
// Устойчивее hash к рекомпиляции: строки UI/конфига чита остаются при пересборке.
func TestStringLiteralMatchType(t *testing.T) {
	svc := NewService(isolatedSigDB(t, "strlit"), "secret", false, nil, "")
	ctx := context.Background()
	if _, err := svc.CreateSignature(ctx, models.CheatSignature{
		Kind: "class", MatchType: "string-literal", Pattern: "killaura", Severity: 8, Enabled: true,
	}); err != nil {
		t.Fatalf("create: %v", err)
	}
	sev, mt := svc.resolveDetection(ctx, "class", "killaura", "")
	if sev != 8 || mt != "string-literal" {
		t.Fatalf("string-literal: ожидалось severity 8 / match_type string-literal, получено sev=%d mt=%q", sev, mt)
	}
	// Не совпавший литерал — дефолт.
	if sev, _ := svc.resolveDetection(ctx, "class", "innocent", ""); sev == 8 {
		t.Fatal("несовпавший литерал не должен давать severity сигнатуры")
	}
}

// string-literal классифицируется как hard (точный сигнал — кандидат на авто-действие).
func TestStringLiteralConfidenceHard(t *testing.T) {
	if c := detectionConfidence("class", "string-literal"); c != "hard" {
		t.Fatalf("string-literal должен быть hard, получено %q", c)
	}
}
