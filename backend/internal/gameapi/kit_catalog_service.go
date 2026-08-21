package gameapi

import (
	"context"
	"errors"
	"regexp"
	"strings"
	"time"

	"launcher-backend/internal/models"

	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

var (
	ErrInvalidKitCatalog = errors.New("invalid kit catalog")
	roleIDPattern        = regexp.MustCompile(`^[a-z0-9_-]{1,64}$`)
)

// KitCatalogService атомарно хранит авторитетный снимок китов игрового сервера.
type KitCatalogService struct {
	db *gorm.DB
}

func NewKitCatalogService(db *gorm.DB) KitCatalogService {
	return KitCatalogService{db: db}
}

func (s KitCatalogService) Replace(ctx context.Context, kits []models.GameKitSnapshot) (int, error) {
	now := time.Now()
	rows := make([]models.GameKitCatalog, 0, len(kits))
	ids := make([]string, 0, len(kits))
	seen := make(map[string]struct{}, len(kits))
	for _, kit := range kits {
		if !validKit(kit) {
			return 0, ErrInvalidKitCatalog
		}
		if _, exists := seen[kit.RoleID]; exists {
			return 0, ErrInvalidKitCatalog
		}
		seen[kit.RoleID] = struct{}{}
		ids = append(ids, kit.RoleID)
		rows = append(rows, models.GameKitCatalog{RoleID: kit.RoleID, Snapshot: kit, UpdatedAt: now})
	}

	err := s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		if len(rows) == 0 {
			return tx.Where("1 = 1").Delete(&models.GameKitCatalog{}).Error
		}
		if err := tx.Clauses(clause.OnConflict{
			Columns:   []clause.Column{{Name: "role_id"}},
			DoUpdates: clause.AssignmentColumns([]string{"snapshot", "updated_at"}),
		}).Create(&rows).Error; err != nil {
			return err
		}
		return tx.Where("role_id NOT IN ?", ids).Delete(&models.GameKitCatalog{}).Error
	})
	return len(rows), err
}

func validKit(kit models.GameKitSnapshot) bool {
	if !roleIDPattern.MatchString(kit.RoleID) || len(kit.DisplayName) > 200 ||
		len(kit.Primary) > 256 || len(kit.Sidearm) > 256 || len(kit.Gear) > 64 || len(kit.Fixed) > 256 {
		return false
	}
	for _, slot := range kit.Gear {
		if strings.TrimSpace(slot.Slot) == "" || len(slot.Slot) > 64 || len(slot.Options) > 256 {
			return false
		}
	}
	return true
}
