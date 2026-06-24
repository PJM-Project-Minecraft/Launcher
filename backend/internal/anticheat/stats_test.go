package anticheat

import (
	"context"
	"testing"
	"time"

	"launcher-backend/internal/models"

	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
)

// SignatureStats агрегирует детекты по сигнатуре: всего срабатываний, уникальных
// игроков и разбивка по статусам review. Это инструмент оценки FP-rate перед autoBan:
// много срабатываний на много игроков при 0 confirmed → сигнатура-ложняк.
func TestSignatureStats(t *testing.T) {
	db, err := gorm.Open(sqlite.Open("file:sig_stats_test?mode=memory&cache=shared"), &gorm.Config{})
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	if err := db.AutoMigrate(&models.Detection{}, &models.Hwid{}, &models.HwidBan{}, &models.AccountBan{}, &models.CheatSignature{}); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	svc := NewService(db, "secret", false, nil, "")
	ctx := context.Background()
	base := time.Unix(1_700_000_000, 0)
	svc.now = func() time.Time { return base }

	// Один и тот же inject от двух разных игроков → 2 срабатывания, 2 уникальных игрока
	// (дедуп по user|session|type|signature; разные игроки → разные ключи).
	r1, _ := svc.InitHandshake(ctx, "uuid-a", "A", "hwid-a", nil)
	c1, _ := svc.VerifyToken(r1.LaunchToken)
	if _, _, err := svc.RecordDetection(ctx, c1, DetectionInput{Type: "inject", Signature: "foreign-agent"}); err != nil {
		t.Fatalf("rec1: %v", err)
	}
	r2, _ := svc.InitHandshake(ctx, "uuid-b", "B", "hwid-b", nil)
	c2, _ := svc.VerifyToken(r2.LaunchToken)
	if _, _, err := svc.RecordDetection(ctx, c2, DetectionInput{Type: "inject", Signature: "foreign-agent"}); err != nil {
		t.Fatalf("rec2: %v", err)
	}

	stats, err := svc.SignatureStats(ctx, base.Add(-time.Hour))
	if err != nil {
		t.Fatalf("stats: %v", err)
	}
	if len(stats) != 1 {
		t.Fatalf("ожидалась одна агрегированная сигнатура, получено %d", len(stats))
	}
	st := stats[0]
	if st.Total != 2 || st.UniquePlayers != 2 {
		t.Fatalf("ожидалось total=2 unique=2, получено total=%d unique=%d", st.Total, st.UniquePlayers)
	}
	if st.Confidence != "hard" {
		t.Fatalf("inject → confidence hard, получено %q", st.Confidence)
	}
	if st.NewCount != 2 {
		t.Fatalf("оба детекта new, получено new=%d", st.NewCount)
	}

	// Подтверждение одного детекта отражается в разбивке статусов.
	var det models.Detection
	db.Where("user_uuid = ?", "uuid-a").First(&det)
	if err := svc.UpdateDetectionStatus(ctx, det.ID, "confirmed", "admin"); err != nil {
		t.Fatalf("update: %v", err)
	}
	stats2, err := svc.SignatureStats(ctx, base.Add(-time.Hour))
	if err != nil {
		t.Fatalf("stats2: %v", err)
	}
	if stats2[0].Confirmed != 1 || stats2[0].NewCount != 1 {
		t.Fatalf("ожидалось confirmed=1 new=1, получено confirmed=%d new=%d", stats2[0].Confirmed, stats2[0].NewCount)
	}
}
