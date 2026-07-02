package anticheat

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"launcher-backend/internal/models"

	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
)

func newScreenshotTestDB(t *testing.T) *gorm.DB {
	t.Helper()
	db, err := gorm.Open(sqlite.Open("file::memory:?cache=shared"), &gorm.Config{})
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	if err := db.AutoMigrate(&models.Screenshot{}); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	return db
}

func TestScreenshotRequestAndPending(t *testing.T) {
	dir := t.TempDir()
	now := time.Now()
	svc := NewScreenshotService(newScreenshotTestDB(t), dir)
	svc.SetNow(func() time.Time { return now })
	ctx := context.Background()

	rec, err := svc.RequestScreenshot(ctx, "uuid-1", "Liko", "nonce-1", "admin")
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	if rec.Status != "pending" || rec.ID == "" || rec.Nonce != "nonce-1" {
		t.Fatalf("неверная запись: %+v", rec)
	}

	// Лаунчер опрашивает — получает pending, статус переходит в capturing.
	got, ok, err := svc.PendingScreenshot(ctx, "nonce-1")
	if err != nil || !ok {
		t.Fatalf("ожидался pending-запрос: ok=%v err=%v", ok, err)
	}
	if got.ID != rec.ID {
		t.Fatalf("ID не совпал: %s vs %s", got.ID, rec.ID)
	}
	if got.Status != "capturing" {
		t.Fatalf("статус должен быть capturing: %s", got.Status)
	}

	// Повторный опрос — больше pending нет (статус уже capturing).
	_, ok2, _ := svc.PendingScreenshot(ctx, "nonce-1")
	if ok2 {
		t.Fatal("не должно быть второго pending для той же записи")
	}

	// Чужой nonce — нет запроса.
	_, ok3, _ := svc.PendingScreenshot(ctx, "nonce-other")
	if ok3 {
		t.Fatal("не должно быть pending для чужого nonce")
	}
}

func TestScreenshotComplete(t *testing.T) {
	dir := t.TempDir()
	now := time.Now()
	svc := NewScreenshotService(newScreenshotTestDB(t), dir)
	svc.SetNow(func() time.Time { return now })
	ctx := context.Background()

	rec, _ := svc.RequestScreenshot(ctx, "uuid-1", "Liko", "nonce-1", "admin")
	_, _, _ = svc.PendingScreenshot(ctx, "nonce-1") // → capturing

	jpeg := []byte{0xFF, 0xD8, 0xFF, 0xE0, 0x00, 0x10, 'J', 'F', 'I', 'F'} // фейковый JPEG-заголовок
	if err := svc.CompleteScreenshot(ctx, rec.ID, jpeg, 1920, 1080); err != nil {
		t.Fatalf("complete: %v", err)
	}

	path, err := svc.ScreenshotFile(ctx, rec.ID)
	if err != nil {
		t.Fatalf("file: %v", err)
	}
	if filepath.Base(path) != rec.ID+".jpg" {
		t.Fatalf("имя файла: %s", filepath.Base(path))
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if len(data) != len(jpeg) {
		t.Fatalf("размер файла: %d vs %d", len(data), len(jpeg))
	}
}

func TestScreenshotReapStale(t *testing.T) {
	dir := t.TempDir()
	t0 := time.Now()
	svc := NewScreenshotService(newScreenshotTestDB(t), dir)
	svc.SetNow(func() time.Time { return t0 })
	ctx := context.Background()

	_, _ = svc.RequestScreenshot(ctx, "uuid-reap", "Liko", "nonce-reap", "admin")
	// Спустя больше TTL — pending должен стать failed.
	svc.SetNow(func() time.Time { return t0.Add(2 * screenshotRequestTTL) })
	svc.reapStale(t0.Add(2 * screenshotRequestTTL))

	var rec models.Screenshot
	if err := svc.db.WithContext(ctx).Where("nonce = ?", "nonce-reap").First(&rec).Error; err != nil {
		t.Fatalf("find: %v", err)
	}
	if rec.Status != "failed" {
		t.Fatalf("ожидался failed после reaper, получил %s", rec.Status)
	}
}

func TestScreenshotRejectsOversize(t *testing.T) {
	dir := t.TempDir()
	svc := NewScreenshotService(newScreenshotTestDB(t), dir)
	ctx := context.Background()

	rec, _ := svc.RequestScreenshot(ctx, "uuid-1", "Liko", "nonce-1", "admin")
	big := make([]byte, MaxScreenshotBytes+1)
	if err := svc.CompleteScreenshot(ctx, rec.ID, big, 1, 1); err == nil {
		t.Fatal("ожидалась ошибка превышения размера")
	}
}

func TestScreenshotReapOldDeletesFiles(t *testing.T) {
	dir := t.TempDir()
	t0 := time.Now()
	svc := NewScreenshotService(newScreenshotTestDB(t), dir)
	svc.SetNow(func() time.Time { return t0 })
	ctx := context.Background()

	// Готовый скриншот + файл на диске.
	rec, _ := svc.RequestScreenshot(ctx, "uuid-old", "Liko", "nonce-old", "admin")
	jpeg := []byte{0xFF, 0xD8, 0xFF}
	if err := svc.CompleteScreenshot(ctx, rec.ID, jpeg, 1, 1); err != nil {
		t.Fatalf("complete: %v", err)
	}
	path, _ := svc.ScreenshotFile(ctx, rec.ID)
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("файл должен существовать: %v", err)
	}

	// Спустя больше retention — файл и запись удаляются.
	svc.SetNow(func() time.Time { return t0.Add(screenshotRetention + time.Hour) })
	svc.reapOld(t0.Add(screenshotRetention + time.Hour))

	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("файл должен быть удалён: %v", err)
	}
	var count int64
	svc.db.WithContext(ctx).Model(&models.Screenshot{}).Where("id = ?", rec.ID).Count(&count)
	if count != 0 {
		t.Fatalf("запись должна быть удалена, count=%d", count)
	}
}

func TestScreenshotFailScreenshot(t *testing.T) {
	dir := t.TempDir()
	svc := NewScreenshotService(newScreenshotTestDB(t), dir)
	ctx := context.Background()

	rec, _ := svc.RequestScreenshot(ctx, "uuid-fail", "Liko", "nonce-fail", "admin")
	if err := svc.FailScreenshot(ctx, rec.ID, "X11 недоступен"); err != nil {
		t.Fatalf("fail: %v", err)
	}
	var got models.Screenshot
	if err := svc.db.WithContext(ctx).Where("id = ?", rec.ID).First(&got).Error; err != nil {
		t.Fatalf("find: %v", err)
	}
	if got.Status != "failed" || got.Error != "X11 недоступен" {
		t.Fatalf("ожидался failed с причиной, получил %s/%q", got.Status, got.Error)
	}
}
