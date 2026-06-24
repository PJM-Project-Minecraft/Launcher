package anticheat

import (
	"context"
	"testing"

	"launcher-backend/internal/models"

	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
)

// isolatedSigDB — изолированная in-memory БД для тестов матчинга сигнатур (общий
// newTestDB шарит cache, и сигнатуры протекали бы между тестами пакета).
func isolatedSigDB(t *testing.T, name string) *gorm.DB {
	t.Helper()
	db, err := gorm.Open(sqlite.Open("file:"+name+"?mode=memory&cache=shared"), &gorm.Config{})
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	if err := db.AutoMigrate(&models.CheatSignature{}); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	return db
}

func TestMatchTypeSubstring(t *testing.T) {
	svc := NewService(isolatedSigDB(t, "mt_substr"), "secret", false, nil, "")
	ctx := context.Background()
	if _, err := svc.CreateSignature(ctx, models.CheatSignature{Kind: "jar", Pattern: "baritone", MatchType: "substring", Severity: 9, Enabled: true}); err != nil {
		t.Fatalf("create: %v", err)
	}
	if sev := svc.resolveSeverity(ctx, "jar", "baritone-1.21.jar"); sev != 9 {
		t.Fatalf("substring должен матчить подстроку, severity %d", sev)
	}
}

func TestMatchTypeExact(t *testing.T) {
	svc := NewService(isolatedSigDB(t, "mt_exact"), "secret", false, nil, "")
	ctx := context.Background()
	if _, err := svc.CreateSignature(ctx, models.CheatSignature{Kind: "process", Pattern: "cheat.exe", MatchType: "exact", Severity: 8, Enabled: true}); err != nil {
		t.Fatalf("create: %v", err)
	}
	if sev := svc.resolveSeverity(ctx, "process", "cheat.exe"); sev != 8 {
		t.Fatalf("exact должен матчить точное имя, severity %d", sev)
	}
	// Ключевое отличие от substring: exact НЕ матчит, если сигнал лишь содержит паттерн.
	if sev := svc.resolveSeverity(ctx, "process", "notcheat.exe"); sev == 8 {
		t.Fatal("exact НЕ должен матчить notcheat.exe (содержит cheat.exe, но не равно)")
	}
}

func TestMatchTypeWord(t *testing.T) {
	svc := NewService(isolatedSigDB(t, "mt_word"), "secret", false, nil, "")
	ctx := context.Background()
	if _, err := svc.CreateSignature(ctx, models.CheatSignature{Kind: "process", Pattern: "xray", MatchType: "word", Severity: 7, Enabled: true}); err != nil {
		t.Fatalf("create: %v", err)
	}
	if sev := svc.resolveSeverity(ctx, "process", "xray client"); sev != 7 {
		t.Fatalf("word должен матчить отдельное слово, severity %d", sev)
	}
	// Главный анти-FP кейс: word НЕ матчит, когда паттерн — часть другого слова.
	if sev := svc.resolveSeverity(ctx, "process", "xrayengine.exe"); sev == 7 {
		t.Fatal("word НЕ должен матчить xrayengine (xray — часть слова)")
	}
}

func TestMatchTypeRegex(t *testing.T) {
	svc := NewService(isolatedSigDB(t, "mt_regex"), "secret", false, nil, "")
	ctx := context.Background()
	if _, err := svc.CreateSignature(ctx, models.CheatSignature{Kind: "process", Pattern: `^wurst-[0-9]+\.jar$`, MatchType: "regex", Severity: 9, Enabled: true}); err != nil {
		t.Fatalf("create: %v", err)
	}
	if sev := svc.resolveSeverity(ctx, "process", "wurst-7.jar"); sev != 9 {
		t.Fatalf("regex должен матчить wurst-7.jar, severity %d", sev)
	}
	if sev := svc.resolveSeverity(ctx, "process", "mywurst-7.jartxt"); sev == 9 {
		t.Fatal("regex с якорями НЕ должен матчить mywurst-7.jartxt")
	}
}

// Невалидный regex отклоняется при сохранении, а не молча игнорируется в рантайме.
func TestRegexValidationRejectsInvalid(t *testing.T) {
	svc := NewService(isolatedSigDB(t, "mt_regexval"), "secret", false, nil, "")
	ctx := context.Background()
	if _, err := svc.CreateSignature(ctx, models.CheatSignature{Kind: "process", Pattern: "[unclosed", MatchType: "regex", Severity: 7, Enabled: true}); err == nil {
		t.Fatal("невалидный regex должен отклоняться при создании сигнатуры")
	}
}

// /rules аддитивно отдаёт match_type и hash (старые клиенты их игнорируют).
func TestRulesIncludesMatchTypeAndHash(t *testing.T) {
	svc := NewService(isolatedSigDB(t, "mt_rules"), "secret", false, nil, "")
	ctx := context.Background()
	if _, err := svc.CreateSignature(ctx, models.CheatSignature{Kind: "class", MatchType: "hash", HashHex: "abc123", Severity: 9, Enabled: true}); err != nil {
		t.Fatalf("create: %v", err)
	}
	rules, err := svc.Rules(ctx)
	if err != nil {
		t.Fatalf("rules: %v", err)
	}
	if len(rules.Signatures) != 1 {
		t.Fatalf("ожидалась 1 сигнатура, получено %d", len(rules.Signatures))
	}
	r := rules.Signatures[0]
	if r.MatchType != "hash" || r.Hash != "abc123" {
		t.Fatalf("Rules должен отдавать match_type и hash, получено matchType=%q hash=%q", r.MatchType, r.Hash)
	}
}

// Дефолт match_type при создании — substring (обратная совместимость со старым поведением).
func TestCreateSignatureDefaultsToSubstring(t *testing.T) {
	svc := NewService(isolatedSigDB(t, "mt_default"), "secret", false, nil, "")
	ctx := context.Background()
	sig, err := svc.CreateSignature(ctx, models.CheatSignature{Kind: "jar", Pattern: "wurst", Severity: 8, Enabled: true})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if sig.MatchType != "substring" {
		t.Fatalf("дефолтный match_type должен быть substring, получено %q", sig.MatchType)
	}
}
