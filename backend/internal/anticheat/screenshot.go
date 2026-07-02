package anticheat

import (
	"context"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"time"

	"launcher-backend/internal/models"

	"github.com/google/uuid"
	"gorm.io/gorm"
)

// screenshotRequestTTL — сколько pending-запрос скриншота ждёт загрузки от
// лаунчера. Лаунчер опрашивает каждые ~5с, так что 60с — с запасом на сетевые
// задержки и несколько пропущенных циклов опроса.
const screenshotRequestTTL = 60 * time.Second

// MaxScreenshotBytes — лимит размера загружаемого JPEG (8 МБ): защищает от
// мусорных/злонамеренных аплоадов. Реальный 1080p JPEG q75 — ~200–500 КБ.
// Экспортируется, чтобы handler мог отвергнуть oversized base64 ДО декодирования.
const MaxScreenshotBytes = 8 << 20

// maxBase64Len — верхняя граница длины base64-строки, в которую укладывается
// MaxScreenshotBytes (base64 раздувает ~4/3). Проверяется до декодирования.
const maxBase64Len = ((MaxScreenshotBytes + 2) / 3) * 4

// ScreenshotService — логика запроса скриншотов экранов игроков: админ создаёт
// pending-запрос по nonce онлайн-сессии, лаунчер опрашивает его, захватывает экран
// и грузит JPEG. Файлы хранятся в storageRoot (storage/screenshots), записи — в БД.
// Резолв nonce → (uuid, login) делает handler через OnlineSessionsProvider, поэтому
// сервис не зависит от yggdrasil и легко тестируется.
type ScreenshotService struct {
	db          *gorm.DB
	storageRoot string
	now         func() time.Time
}

func NewScreenshotService(db *gorm.DB, storageRoot string) *ScreenshotService {
	return &ScreenshotService{
		db:          db,
		storageRoot: storageRoot,
		now:         time.Now,
	}
}

// SetNow инъектирует часы (детерминированные тесты).
func (s *ScreenshotService) SetNow(now func() time.Time) {
	if now != nil {
		s.now = now
	}
}

// RequestScreenshot создаёт pending-запрос скриншота для онлайн-сессии по nonce.
// UUID/login передаёт handler (резолв через yggdrasil.Store). Возвращает запись
// (с ID, по которому дашборд будет поллить статус).
func (s *ScreenshotService) RequestScreenshot(ctx context.Context, userUUID, login, nonce, adminLogin string) (models.Screenshot, error) {
	if userUUID == "" || nonce == "" {
		return models.Screenshot{}, errors.New("userUuid и nonce обязательны")
	}
	rec := models.Screenshot{
		ID:          uuid.NewString(),
		UserUUID:    userUUID,
		Login:       login,
		Nonce:       nonce,
		Status:      "pending",
		RequestedBy: adminLogin,
		CreatedAt:   s.now(),
	}
	if err := s.db.WithContext(ctx).Create(&rec).Error; err != nil {
		return models.Screenshot{}, err
	}
	slog.Info("screenshot requested", "uuid", userUUID, "login", login, "id", rec.ID, "by", adminLogin)
	return rec, nil
}

// PendingScreenshot возвращает активный pending-запрос для лаунчера по nonce
// (лаунчер опрашивает по launch-token → claims.Nonce). Переводит в "capturing",
// чтобы два цикла опроса не взяли запрос дважды. Нет запроса → (zero, false).
func (s *ScreenshotService) PendingScreenshot(ctx context.Context, nonce string) (models.Screenshot, bool, error) {
	if nonce == "" {
		return models.Screenshot{}, false, nil
	}
	var rec models.Screenshot
	err := s.db.WithContext(ctx).
		Where("nonce = ? AND status = ?", nonce, "pending").
		Order("created_at asc").
		First(&rec).Error
	if err != nil {
		return models.Screenshot{}, false, nil // not found — нет запроса
	}
	if err := s.db.WithContext(ctx).Model(&models.Screenshot{}).Where("id = ?", rec.ID).
		Updates(map[string]any{"status": "capturing"}).Error; err != nil {
		return models.Screenshot{}, false, err
	}
	rec.Status = "capturing"
	return rec, true, nil
}

// CompleteScreenshot сохраняет загруженный лаунчером JPEG: пишет файл в storageRoot
// и помечает запись done. Меняет статус на failed при ошибке записи.
func (s *ScreenshotService) CompleteScreenshot(ctx context.Context, id string, data []byte, width, height int) error {
	if len(data) == 0 {
		return errors.New("пустой скриншот")
	}
	if len(data) > MaxScreenshotBytes {
		return errors.New("скриншот слишком большой")
	}
	var rec models.Screenshot
	if err := s.db.WithContext(ctx).Where("id = ?", id).First(&rec).Error; err != nil {
		return errors.New("скриншот не найден")
	}
	if rec.Status != "capturing" && rec.Status != "pending" {
		return errors.New("скриншот уже обработан")
	}
	if err := os.MkdirAll(s.storageRoot, 0o755); err != nil {
		s.markFailed(ctx, id, "не удалось создать папку")
		return err
	}
	fileName := rec.ID + ".jpg"
	path := filepath.Join(s.storageRoot, fileName)
	if err := os.WriteFile(path, data, 0o644); err != nil {
		s.markFailed(ctx, id, "ошибка записи файла")
		return err
	}
	now := s.now()
	return s.db.WithContext(ctx).Model(&models.Screenshot{}).Where("id = ?", id).
		Updates(map[string]any{
			"status":      "done",
			"file_name":   fileName,
			"width":       width,
			"height":      height,
			"size":        int64(len(data)),
			"captured_at": now,
			"error":       "",
		}).Error
}

// markFailed помечает запрос проваленным (ошибка захвата/загрузки).
func (s *ScreenshotService) markFailed(ctx context.Context, id, reason string) {
	_ = s.db.WithContext(ctx).Model(&models.Screenshot{}).Where("id = ?", id).
		Updates(s.failedUpdates(reason)).Error
}

// failedUpdates — единая схема failed-апдейта (источник истины для markFailed и
// reapStale), чтобы таймаут-записи не расходились с активно-проваленными при
// эволюции схемы (например, при добавлении failed_at).
func (s *ScreenshotService) failedUpdates(reason string) map[string]any {
	return map[string]any{"status": "failed", "error": reason}
}

// FailScreenshot — публичный путь: лаунчер может сообщить, что не смог захватить экран.
func (s *ScreenshotService) FailScreenshot(ctx context.Context, id, reason string) error {
	if reason == "" {
		reason = "захват не удался"
	}
	s.markFailed(ctx, id, reason)
	return nil
}

// ListScreenshots — последние скриншоты для дашборда (вкл. pending/failed для статуса).
func (s *ScreenshotService) ListScreenshots(ctx context.Context, limit int) ([]models.Screenshot, error) {
	if limit <= 0 || limit > 500 {
		limit = 100
	}
	var out []models.Screenshot
	err := s.db.WithContext(ctx).Order("created_at desc").Limit(limit).Find(&out).Error
	return out, err
}

// ScreenshotFile возвращает путь к JPEG-файлу для отдачи в дашборд. Только done.
func (s *ScreenshotService) ScreenshotFile(ctx context.Context, id string) (string, error) {
	var rec models.Screenshot
	if err := s.db.WithContext(ctx).Where("id = ?", id).First(&rec).Error; err != nil {
		return "", errors.New("скриншот не найден")
	}
	if rec.Status != "done" || rec.FileName == "" {
		return "", errors.New("скриншот недоступен")
	}
	return filepath.Join(s.storageRoot, rec.FileName), nil
}

// BelongsToNonce проверяет, что скриншот принадлежит игровой сессии с этим nonce.
// Защита от загрузки чужого скриншота по перехваченному ID: лаунчер грузит только
// скриншоты своей сессии (nonce из launch-token).
func (s *ScreenshotService) BelongsToNonce(ctx context.Context, id, nonce string) bool {
	if id == "" || nonce == "" {
		return false
	}
	var count int64
	s.db.WithContext(ctx).Model(&models.Screenshot{}).
		Where("id = ? AND nonce = ?", id, nonce).Count(&count)
	return count > 0
}

// reapStale помечает pending/capturing-запросы, не завершённые за screenshotRequestTTL,
// как failed. Вызывается фоновым reaper'ом, как и heartbeat-reaper. Использует
// failedUpdates — общую схему с markFailed, чтобы таймаут-записи не расходились.
func (s *ScreenshotService) reapStale(now time.Time) {
	threshold := now.Add(-screenshotRequestTTL)
	err := s.db.Model(&models.Screenshot{}).
		Where("status IN ? AND created_at < ?", []string{"pending", "capturing"}, threshold).
		Updates(s.failedUpdates("таймаут ожидания лаунчера")).Error
	if err != nil {
		slog.Warn("screenshot: reap stale failed", "error", err)
	}
}

// screenshotRetention — сколько хранить завершённые (done/failed) скриншоты и
// их файлы на диске. Старше — удаляем (защита 40ГБ VPS от переполнения).
const screenshotRetention = 30 * 24 * time.Hour

// reapOld удаляет JPEG-файлы и БД-записи скриншотов старше screenshotRetention.
// Сначала remove файлов, потом DELETE — orphan-файлы при сбое БД безопаснее, чем
// orphan-записи без файлов (дашборд просто покажет «недоступен», а не мусор на диске).
func (s *ScreenshotService) reapOld(now time.Time) {
	threshold := now.Add(-screenshotRetention)
	var old []models.Screenshot
	if err := s.db.Where("created_at < ?", threshold).Find(&old).Error; err != nil {
		slog.Warn("screenshot: reapOld select failed", "error", err)
		return
	}
	for _, rec := range old {
		if rec.FileName != "" {
			_ = os.Remove(filepath.Join(s.storageRoot, rec.FileName))
		}
	}
	if err := s.db.Where("created_at < ?", threshold).Delete(&models.Screenshot{}).Error; err != nil {
		slog.Warn("screenshot: reapOld delete failed", "error", err)
	}
}

// StartReaper запускает фоновую чистку протухших pending-запросов и старых
// скриншотов (один раз из main.go). Интервал — как у heartbeat-reaper.
func (s *ScreenshotService) StartReaper(interval time.Duration) {
	go func() {
		for range time.Tick(interval) {
			now := s.now()
			s.reapStale(now)
			s.reapOld(now)
		}
	}()
}
