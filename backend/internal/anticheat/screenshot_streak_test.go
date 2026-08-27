package anticheat

import (
	"testing"
	"time"

	"launcher-backend/internal/models"
)

// requestAndTimeout — запрос скриншота, который никто не забрал: reaper обязан
// пометить его failed и сообщить детектору.
func requestAndTimeout(t *testing.T, shots *ScreenshotService, uuid, login, nonce string, now time.Time) {
	t.Helper()
	if _, err := shots.RequestScreenshot(t.Context(), uuid, login, nonce, "admin"); err != nil {
		t.Fatalf("request: %v", err)
	}
	shots.reapStale(now.Add(screenshotRequestTTL + time.Minute))
}

func TestScreenshotFailStreakRaisesDetection(t *testing.T) {
	db := newTestDB(t)
	if err := db.AutoMigrate(&models.Screenshot{}); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	svc := NewService(db, "secret", false, nil, "")
	shots := NewScreenshotService(db, t.TempDir())
	shots.SetDetector(svc)

	now := time.Now()
	uuid, nonce := "uuid-shotfail", "n-shotfail"
	for i := 0; i < screenshotFailStreak-1; i++ {
		requestAndTimeout(t, shots, uuid, "Liko", nonce, now)
	}
	var count int64
	db.Model(&models.Detection{}).Where("user_uuid = ?", uuid).Count(&count)
	if count != 0 {
		t.Fatalf("до порога детекта быть не должно, есть %d", count)
	}

	// Порог: третий провал подряд.
	requestAndTimeout(t, shots, uuid, "Liko", nonce, now)
	var detections []models.Detection
	if err := db.Where("user_uuid = ?", uuid).Find(&detections).Error; err != nil {
		t.Fatalf("select: %v", err)
	}
	if len(detections) != 1 {
		t.Fatalf("ожидался один детект на серии из %d, получено %d", screenshotFailStreak, len(detections))
	}
	d := detections[0]
	if d.Type != screenshotFailType || d.Signature != "screenshot-timeout" {
		t.Fatalf("молчание клиента должно писаться как screenshot-timeout: %+v", d)
	}
	// Кикать за это нельзя: у части игроков захват не работает по среде.
	if d.Confidence != "soft" {
		t.Fatalf("детект обязан быть soft (без кика/бана), получено %q", d.Confidence)
	}
	if kick, _ := svc.EvaluateKick(LaunchClaims{UUID: uuid, Nonce: nonce}, d.Severity, d.Confidence, d.Type); kick {
		t.Fatal("серия провалов не должна кикать игрока")
	}
}

func TestScreenshotSuccessResetsStreak(t *testing.T) {
	db := newTestDB(t)
	if err := db.AutoMigrate(&models.Screenshot{}); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	svc := NewService(db, "secret", false, nil, "")
	shots := NewScreenshotService(db, t.TempDir())
	shots.SetDetector(svc)

	now := time.Now()
	uuid, nonce := "uuid-shotok", "n-shotok"
	requestAndTimeout(t, shots, uuid, "Bob", nonce, now)
	requestAndTimeout(t, shots, uuid, "Bob", nonce, now)

	// Удачный кадр между провалами обнуляет серию — иначе редкие сетевые сбои за вечер
	// накопились бы в ложный детект.
	rec, err := shots.RequestScreenshot(t.Context(), uuid, "Bob", nonce, "admin")
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	jpegData := []byte{0xFF, 0xD8, 0xFF, 0xE0, 0x00}
	if err := shots.CompleteScreenshot(t.Context(), rec.ID, jpegData, 4, 4, "x11"); err != nil {
		t.Fatalf("complete: %v", err)
	}

	requestAndTimeout(t, shots, uuid, "Bob", nonce, now)
	var count int64
	db.Model(&models.Detection{}).Where("user_uuid = ?", uuid).Count(&count)
	if count != 0 {
		t.Fatalf("после удачного кадра серия должна начинаться заново, детектов: %d", count)
	}
}
