package anticheat

import (
	"context"
	"testing"

	"launcher-backend/internal/models"

	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
)

func newTestDB(t *testing.T) *gorm.DB {
	t.Helper()
	db, err := gorm.Open(sqlite.Open("file::memory:?cache=shared"), &gorm.Config{})
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	if err := db.AutoMigrate(&models.Detection{}, &models.Hwid{}, &models.HwidBan{}, &models.AccountBan{}, &models.CheatSignature{}); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	return db
}

func TestInitHandshakeIssuesToken(t *testing.T) {
	svc := NewService(newTestDB(t), "secret", false, nil, "")
	ctx := context.Background()

	res, err := svc.InitHandshake(ctx, "uuid-1", "Liko", "hwid-1", nil)
	if err != nil {
		t.Fatalf("init: %v", err)
	}
	if !res.Allowed || res.LaunchToken == "" || res.Nonce == "" {
		t.Fatalf("ожидался разрешённый запуск с токеном: %+v", res)
	}
	claims, err := svc.VerifyToken(res.LaunchToken)
	if err != nil {
		t.Fatalf("verify: %v", err)
	}
	if claims.UUID != "uuid-1" || claims.Nonce != res.Nonce {
		t.Fatalf("claims не совпали: %+v", claims)
	}
}

func TestInitHandshakeBlocksBannedAccount(t *testing.T) {
	svc := NewService(newTestDB(t), "secret", false, nil, "")
	ctx := context.Background()

	if err := svc.BanAccount(ctx, "uuid-ban", "Cheater", "x-ray", "admin"); err != nil {
		t.Fatalf("ban: %v", err)
	}
	res, err := svc.InitHandshake(ctx, "uuid-ban", "Cheater", "hwid-2", nil)
	if err != nil {
		t.Fatalf("init: %v", err)
	}
	if res.Allowed || res.LaunchToken != "" {
		t.Fatalf("забаненный аккаунт не должен получать токен: %+v", res)
	}
}

func TestInitHandshakeBlocksBannedHwid(t *testing.T) {
	svc := NewService(newTestDB(t), "secret", false, nil, "")
	ctx := context.Background()

	if err := svc.BanHwid(ctx, "hwid-bad", "cheat device", "admin"); err != nil {
		t.Fatalf("ban: %v", err)
	}
	res, _ := svc.InitHandshake(ctx, "uuid-3", "Bob", "hwid-bad", nil)
	if res.Allowed {
		t.Fatal("забаненный HWID не должен получать токен")
	}
}

func TestInitRecordsHwidAndDetections(t *testing.T) {
	db := newTestDB(t)
	svc := NewService(db, "secret", false, nil, "")
	ctx := context.Background()

	_, err := svc.InitHandshake(ctx, "uuid-4", "Eve", "hwid-4", []DetectionInput{
		{Type: "process", Signature: "cheatengine.exe", Severity: 6},
	})
	if err != nil {
		t.Fatalf("init: %v", err)
	}

	var hwidCount, detCount int64
	db.Model(&models.Hwid{}).Where("hash = ?", "hwid-4").Count(&hwidCount)
	db.Model(&models.Detection{}).Where("user_uuid = ?", "uuid-4").Count(&detCount)
	if hwidCount != 1 {
		t.Fatalf("HWID не записан: %d", hwidCount)
	}
	if detCount != 1 {
		t.Fatalf("детект не записан: %d", detCount)
	}
}

type fakeVerifier struct {
	verified map[string]bool
}

func (f *fakeVerifier) MarkVerifiedByNonce(nonce string) bool {
	if f.verified[nonce] {
		return false // одноразовость
	}
	f.verified[nonce] = true
	return nonce != ""
}

func (f *fakeVerifier) InvalidateByNonce(nonce string) bool { return nonce != "" }

func TestConfirmMarksSessionVerified(t *testing.T) {
	verifier := &fakeVerifier{verified: map[string]bool{}}
	svc := NewService(newTestDB(t), "secret", false, verifier, "")
	ctx := context.Background()

	res, _ := svc.InitHandshake(ctx, "uuid-c", "Liko", "hwid-c", nil)
	if err := svc.Confirm(res.LaunchToken); err != nil {
		t.Fatalf("confirm: %v", err)
	}
	if !verifier.verified[res.Nonce] {
		t.Fatal("confirm должен пометить сессию по nonce")
	}
	// Повторный confirm по тому же токену/nonce не проходит.
	if err := svc.Confirm(res.LaunchToken); err == nil {
		t.Fatal("повторный confirm должен быть отклонён")
	}
}

func TestConfirmRejectsBadToken(t *testing.T) {
	svc := NewService(newTestDB(t), "secret", false, &fakeVerifier{verified: map[string]bool{}}, "")
	if err := svc.Confirm("garbage.token"); err == nil {
		t.Fatal("невалидный токен не должен подтверждаться")
	}
}

func TestAutoBanOnHighSeverity(t *testing.T) {
	db := newTestDB(t)
	svc := NewService(db, "secret", true, nil, "") // autoBan включён
	ctx := context.Background()

	// Severity теперь СЕРВЕРНАЯ: заводим сигнатуру блэклиста c severity 9.
	if _, err := svc.CreateSignature(ctx, models.CheatSignature{Kind: "jar", Pattern: "baritone", Severity: 9, Enabled: true}); err != nil {
		t.Fatalf("signature: %v", err)
	}

	res, _ := svc.InitHandshake(ctx, "uuid-5", "Mal", "hwid-5", nil)
	claims, _ := svc.VerifyToken(res.LaunchToken)
	// Клиент шлёт заниженную severity=1 — сервер обязан её проигнорировать и взять 9 из блэклиста.
	sev, err := svc.RecordDetection(ctx, claims, DetectionInput{Type: "jar", Signature: "baritone-1.21.jar", Severity: 1})
	if err != nil {
		t.Fatalf("detect: %v", err)
	}
	if sev < autoBanSeverity {
		t.Fatalf("серверная severity должна быть >= %d (из блэклиста), получено %d", autoBanSeverity, sev)
	}

	var accBans, hwidBans int64
	db.Model(&models.AccountBan{}).Where("user_uuid = ?", "uuid-5").Count(&accBans)
	db.Model(&models.HwidBan{}).Where("hwid_hash = ?", "hwid-5").Count(&hwidBans)
	if accBans != 1 || hwidBans != 1 {
		t.Fatalf("ожидался авто-бан аккаунта и HWID: acc=%d hwid=%d", accBans, hwidBans)
	}
	// Первое нарушение — временный бан (эскалация): ExpiresAt должен быть установлен.
	var accBan models.AccountBan
	db.Where("user_uuid = ?", "uuid-5").First(&accBan)
	if accBan.ExpiresAt == nil {
		t.Fatal("первый авто-бан должен быть временным (ExpiresAt != nil)")
	}
}

func TestAutoBanEscalatesToPermanent(t *testing.T) {
	db := newTestDB(t)
	svc := NewService(db, "secret", true, nil, "")
	ctx := context.Background()
	if _, err := svc.CreateSignature(ctx, models.CheatSignature{Kind: "jar", Pattern: "baritone", Severity: 9, Enabled: true}); err != nil {
		t.Fatalf("signature: %v", err)
	}
	// Одна сессия (claims переиспользуются): RecordDetection не проверяет баны, в отличие
	// от InitHandshake, поэтому эскалация воспроизводится в рамках одного игрового запуска.
	res, _ := svc.InitHandshake(ctx, "uuid-esc", "Mal", "hwid-esc", nil)
	claims, _ := svc.VerifyToken(res.LaunchToken)

	if _, err := svc.RecordDetection(ctx, claims, DetectionInput{Type: "jar", Signature: "baritone-a.jar"}); err != nil {
		t.Fatalf("detect1: %v", err)
	}
	var b1 models.AccountBan
	db.Where("user_uuid = ?", "uuid-esc").First(&b1)
	if b1.ExpiresAt == nil {
		t.Fatal("первый авто-бан должен быть временным (ExpiresAt != nil)")
	}

	if _, err := svc.RecordDetection(ctx, claims, DetectionInput{Type: "jar", Signature: "baritone-b.jar"}); err != nil {
		t.Fatalf("detect2: %v", err)
	}
	var b2 models.AccountBan
	db.Where("user_uuid = ?", "uuid-esc").First(&b2)
	if b2.ExpiresAt != nil {
		t.Fatal("повторный авто-бан должен стать перманентным (ExpiresAt == nil)")
	}
}

func TestServerSeverityForSystemType(t *testing.T) {
	svc := NewService(newTestDB(t), "secret", false, nil, "")
	ctx := context.Background()
	// Системный тип inject — severity из серверной карты (9), не из клиента.
	if got := svc.resolveSeverity(ctx, "inject", "anything"); got != 9 {
		t.Fatalf("inject должен иметь серверную severity 9, получено %d", got)
	}
	// Неизвестная сигнатура без блэклиста — дефолт.
	if got := svc.resolveSeverity(ctx, "process", "unknown.exe"); got != defaultDetectionSeverity {
		t.Fatalf("несовпавшая сигнатура должна давать дефолт %d, получено %d", defaultDetectionSeverity, got)
	}
}
