package repo

import (
	"context"
	"errors"
	"strings"
	"time"

	"launcher-backend/internal/models"

	"gorm.io/gorm"
)

// AdminStats — сводка для дашборда.
type AdminStats struct {
	TotalUsers     int64 `json:"totalUsers"`
	TelegramLinked int64 `json:"telegramLinked"`
	BannedUsers    int64 `json:"bannedUsers"`
	HwidBanned     int64 `json:"hwidBanned"`
	NewUsers7d     int64 `json:"newUsers7d"`
	AuthSuccess24h int64 `json:"authSuccess24h"`
	AuthFailure24h int64 `json:"authFailure24h"`
}

func FetchAdminStats(ctx context.Context, db *gorm.DB) (*AdminStats, error) {
	var s AdminStats
	u := func() *gorm.DB { return db.WithContext(ctx).Model(&models.User{}) }
	if err := u().Count(&s.TotalUsers).Error; err != nil {
		return nil, err
	}
	if err := u().Where("telegram_id IS NOT NULL").Count(&s.TelegramLinked).Error; err != nil {
		return nil, err
	}
	if err := u().Where("is_banned = ?", true).Count(&s.BannedUsers).Error; err != nil {
		return nil, err
	}
	if err := u().Where("is_hwid_banned = ?", true).Count(&s.HwidBanned).Error; err != nil {
		return nil, err
	}
	weekAgo := time.Now().UTC().Add(-7 * 24 * time.Hour)
	if err := u().Where("created_at >= ?", weekAgo).Count(&s.NewUsers7d).Error; err != nil {
		return nil, err
	}
	dayAgo := time.Now().UTC().Add(-24 * time.Hour)
	db.WithContext(ctx).Model(&models.AuthLog{}).Where("created_at >= ? AND success = ?", dayAgo, true).Count(&s.AuthSuccess24h)
	db.WithContext(ctx).Model(&models.AuthLog{}).Where("created_at >= ? AND success = ?", dayAgo, false).Count(&s.AuthFailure24h)
	return &s, nil
}

// AdminUserListItem — строка списка пользователей.
type AdminUserListItem struct {
	models.User
}

const AdminPageSize = 30

func ListUsersAdmin(ctx context.Context, db *gorm.DB, q string, page int) ([]AdminUserListItem, int64, error) {
	if page < 1 {
		page = 1
	}
	base := db.WithContext(ctx).Model(&models.User{})
	if q != "" {
		// LOWER + CAST: поиск регистронезависимый, а сравнение uuid-колонки с
		// произвольной строкой не роняет запрос в Postgres (id имеет тип uuid).
		like := "%" + strings.ToLower(q) + "%"
		base = base.Where(
			"LOWER(login) LIKE ? OR LOWER(email) LIKE ? OR CAST(id AS TEXT) = ? OR provider_uuid = ?",
			like, like, strings.ToLower(q), q,
		)
	}
	var total int64
	if err := base.Count(&total).Error; err != nil {
		return nil, 0, err
	}
	var users []models.User
	if err := base.Order("created_at DESC").Limit(AdminPageSize).Offset((page - 1) * AdminPageSize).Find(&users).Error; err != nil {
		return nil, 0, err
	}
	out := make([]AdminUserListItem, 0, len(users))
	for _, u := range users {
		out = append(out, AdminUserListItem{User: u})
	}
	return out, total, nil
}

// AdminUserDetail — карточка пользователя для дашборда.
type AdminUserDetail struct {
	User      models.User          `json:"user"`
	AuthLogs  []models.AuthLog     `json:"authLogs"`
	AuditLogs []models.BotAuditLog `json:"auditLogs"`
}

func GetUserDetail(ctx context.Context, db *gorm.DB, id string) (*AdminUserDetail, error) {
	u, err := FindUserByID(ctx, db, id)
	if err != nil {
		return nil, err
	}
	if u == nil {
		return nil, nil
	}
	out := &AdminUserDetail{User: *u}
	db.WithContext(ctx).Where("user_id = ?", id).Order("created_at DESC").Limit(50).Find(&out.AuthLogs)
	db.WithContext(ctx).Where("target_user_id = ?", id).Order("created_at DESC").Limit(20).Find(&out.AuditLogs)
	return out, nil
}

func ValidRole(role string) bool {
	switch role {
	case models.RoleUser, models.RoleModerator, models.RoleAdmin:
		return true
	default:
		return false
	}
}

func UpdateUserRole(ctx context.Context, db *gorm.DB, id, role string) (bool, error) {
	if !ValidRole(role) {
		return false, errors.New("invalid role")
	}
	res := db.WithContext(ctx).Model(&models.User{}).Where("id = ?", id).
		Updates(map[string]any{"role": role, "updated_at": time.Now().UTC()})
	return res.RowsAffected > 0, res.Error
}

func SetBan(ctx context.Context, db *gorm.DB, id string, banned bool) (bool, error) {
	res := db.WithContext(ctx).Model(&models.User{}).Where("id = ?", id).
		Updates(map[string]any{"is_banned": banned, "updated_at": time.Now().UTC()})
	return res.RowsAffected > 0, res.Error
}

func SetHwidBan(ctx context.Context, db *gorm.DB, id string, banned bool) (bool, error) {
	res := db.WithContext(ctx).Model(&models.User{}).Where("id = ?", id).
		Updates(map[string]any{"is_hwid_banned": banned, "updated_at": time.Now().UTC()})
	return res.RowsAffected > 0, res.Error
}

func DeleteUser(ctx context.Context, db *gorm.DB, id string) error {
	return db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		// auth_logs не имеют FK-каскада: чистим вручную.
		if err := tx.Where("user_id = ?", id).Delete(&models.AuthLog{}).Error; err != nil {
			return err
		}
		return tx.Where("id = ?", id).Delete(&models.User{}).Error
	})
}

func ListAuthLogs(ctx context.Context, db *gorm.DB, page int) ([]models.AuthLog, error) {
	if page < 1 {
		page = 1
	}
	var out []models.AuthLog
	err := db.WithContext(ctx).Order("created_at DESC").Limit(AdminPageSize).Offset((page - 1) * AdminPageSize).Find(&out).Error
	return out, err
}

func ListAuditLogs(ctx context.Context, db *gorm.DB, page int) ([]models.BotAuditLog, error) {
	if page < 1 {
		page = 1
	}
	var out []models.BotAuditLog
	err := db.WithContext(ctx).Order("created_at DESC").Limit(AdminPageSize).Offset((page - 1) * AdminPageSize).Find(&out).Error
	return out, err
}
