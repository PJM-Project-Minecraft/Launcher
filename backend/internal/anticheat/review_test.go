package anticheat

import (
	"context"
	"testing"

	"launcher-backend/internal/models"

	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
)

// Новый детект получает статус "new" и серверную confidence (hard/soft) — основа
// review-очереди: оператор видит уверенность сигнала и разбирает «new».
func TestRecordDetectionSetsStatusAndConfidence(t *testing.T) {
	db := newTestDB(t)
	svc := NewService(db, "secret", false, nil, "")
	ctx := context.Background()
	res, _ := svc.InitHandshake(ctx, "uuid-st", "Liko", "hwid-st", nil)
	claims, _ := svc.VerifyToken(res.LaunchToken)

	if _, _, err := svc.RecordDetection(ctx, claims, DetectionInput{Type: "inject", Signature: "foreign-agent"}); err != nil {
		t.Fatalf("detect inject: %v", err)
	}
	if _, _, err := svc.RecordDetection(ctx, claims, DetectionInput{Type: "module-unknown", Signature: "weird.so"}); err != nil {
		t.Fatalf("detect module: %v", err)
	}

	var injectDet, moduleDet models.Detection
	db.Where("user_uuid = ? AND type = ?", "uuid-st", "inject").First(&injectDet)
	db.Where("user_uuid = ? AND type = ?", "uuid-st", "module-unknown").First(&moduleDet)

	if injectDet.Status != "new" {
		t.Fatalf("новый детект должен иметь статус new, получено %q", injectDet.Status)
	}
	if injectDet.Confidence != "hard" {
		t.Fatalf("inject должен быть hard, получено %q", injectDet.Confidence)
	}
	if moduleDet.Confidence != "soft" {
		t.Fatalf("module-unknown должен быть soft, получено %q", moduleDet.Confidence)
	}
}

// Оператор подтверждает/отклоняет детект: статус меняется, фиксируется кто и когда.
func TestUpdateDetectionStatus(t *testing.T) {
	db := newTestDB(t)
	svc := NewService(db, "secret", false, nil, "")
	ctx := context.Background()
	res, _ := svc.InitHandshake(ctx, "uuid-up", "Liko", "hwid-up", nil)
	claims, _ := svc.VerifyToken(res.LaunchToken)
	if _, _, err := svc.RecordDetection(ctx, claims, DetectionInput{Type: "inject", Signature: "foreign-agent"}); err != nil {
		t.Fatalf("detect: %v", err)
	}
	var det models.Detection
	db.Where("user_uuid = ?", "uuid-up").First(&det)

	if err := svc.UpdateDetectionStatus(ctx, det.ID, "confirmed", "admin"); err != nil {
		t.Fatalf("update: %v", err)
	}
	var updated models.Detection
	db.Where("id = ?", det.ID).First(&updated)
	if updated.Status != "confirmed" {
		t.Fatalf("статус должен стать confirmed, получено %q", updated.Status)
	}
	if updated.ReviewedBy != "admin" {
		t.Fatalf("ReviewedBy должен быть admin, получено %q", updated.ReviewedBy)
	}
	if updated.ReviewedAt == nil {
		t.Fatal("ReviewedAt должен быть установлен")
	}
}

// Невалидный статус отклоняется (защита от мусора в review-поле).
func TestUpdateDetectionStatusRejectsInvalid(t *testing.T) {
	db := newTestDB(t)
	svc := NewService(db, "secret", false, nil, "")
	ctx := context.Background()
	res, _ := svc.InitHandshake(ctx, "uuid-inv", "Liko", "hwid-inv", nil)
	claims, _ := svc.VerifyToken(res.LaunchToken)
	if _, _, err := svc.RecordDetection(ctx, claims, DetectionInput{Type: "inject", Signature: "foreign-agent"}); err != nil {
		t.Fatalf("detect: %v", err)
	}
	var det models.Detection
	db.Where("user_uuid = ?", "uuid-inv").First(&det)
	if err := svc.UpdateDetectionStatus(ctx, det.ID, "garbage", "admin"); err == nil {
		t.Fatal("невалидный статус должен отклоняться")
	}
}

// Фильтр review-очереди по confidence: оператор смотрит soft-детекты отдельно.
func TestListDetectionsFilterByConfidence(t *testing.T) {
	// Изолированная БД: детекты не должны смешиваться с другими тестами (shared cache).
	db, err := gorm.Open(sqlite.Open("file:list_det_filter_test?mode=memory&cache=shared"), &gorm.Config{})
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	if err := db.AutoMigrate(&models.Detection{}, &models.Hwid{}, &models.HwidBan{}, &models.AccountBan{}, &models.CheatSignature{}); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	svc := NewService(db, "secret", false, nil, "")
	ctx := context.Background()
	res, _ := svc.InitHandshake(ctx, "uuid-lf", "Liko", "hwid-lf", nil)
	claims, _ := svc.VerifyToken(res.LaunchToken)
	if _, _, err := svc.RecordDetection(ctx, claims, DetectionInput{Type: "inject", Signature: "foreign-agent"}); err != nil {
		t.Fatalf("detect hard: %v", err)
	}
	if _, _, err := svc.RecordDetection(ctx, claims, DetectionInput{Type: "module-unknown", Signature: "weird.so"}); err != nil {
		t.Fatalf("detect soft: %v", err)
	}

	soft, err := svc.ListDetections(ctx, 100, DetectionFilter{Confidence: "soft"})
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(soft) != 1 || soft[0].Type != "module-unknown" {
		t.Fatalf("фильтр confidence=soft должен вернуть только module-unknown, получено %+v", soft)
	}
	all, err := svc.ListDetections(ctx, 100, DetectionFilter{})
	if err != nil {
		t.Fatalf("list all: %v", err)
	}
	if len(all) != 2 {
		t.Fatalf("без фильтра должно быть 2 детекта, получено %d", len(all))
	}
}
