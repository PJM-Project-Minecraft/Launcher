package models_test

import (
	"testing"
	"time"

	"launcher-backend/internal/models"

	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
)

func TestPlayerProfileAndAdjustmentMigrate(t *testing.T) {
	db, err := gorm.Open(sqlite.Open("file:gameplayer_test?mode=memory&cache=shared"), &gorm.Config{})
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	if err := db.AutoMigrate(&models.PlayerProfile{}, &models.PlayerXpAdjustment{}); err != nil {
		t.Fatalf("migrate: %v", err)
	}

	p := models.PlayerProfile{UUID: "11111111-1111-1111-1111-111111111111", Name: "Liko", XP: 500, RankID: "sergeant"}
	if err := db.Create(&p).Error; err != nil {
		t.Fatalf("create profile: %v", err)
	}

	set := int64(1000)
	adj := models.PlayerXpAdjustment{UUID: p.UUID, Delta: 0, SetValue: &set, Reason: "проверка", CreatedBy: "admin"}
	if err := db.Create(&adj).Error; err != nil {
		t.Fatalf("create adjustment: %v", err)
	}
	if adj.ID == 0 {
		t.Fatal("ID правки должен проставляться автоинкрементом")
	}
	if adj.AppliedAt != nil {
		t.Fatal("новая правка обязана быть неприменённой (applied_at IS NULL)")
	}

	now := time.Now()
	if err := db.Model(&adj).Update("applied_at", now).Error; err != nil {
		t.Fatalf("ack: %v", err)
	}

	var pending int64
	db.Model(&models.PlayerXpAdjustment{}).Where("applied_at IS NULL").Count(&pending)
	if pending != 0 {
		t.Fatalf("после ACK неприменённых правок быть не должно, получено %d", pending)
	}
}
