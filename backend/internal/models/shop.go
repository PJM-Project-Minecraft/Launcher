package models

import "time"

// ShopItem — единый источник истины для витрины, денежного пути и игровой
// выдачи. Price хранится в копейках.
type ShopItem struct {
	ID           int64        `gorm:"primaryKey;autoIncrement:false" json:"id"`
	Category     string       `gorm:"size:100;index;not null" json:"category"`
	CategoryIcon string       `gorm:"size:64;not null" json:"categoryIcon"`
	Name         string       `gorm:"size:300;not null" json:"name"`
	Description  string       `gorm:"type:text;not null" json:"description"`
	Price        int64        `gorm:"not null" json:"price"`
	ImagePath    string       `gorm:"size:500;not null" json:"-"`
	Badge        string       `gorm:"size:80;not null" json:"badge,omitempty"`
	Sort         int          `gorm:"index;not null" json:"sort"`
	Active       bool         `gorm:"index;not null" json:"active"`
	Delivery     DeliverySpec `gorm:"serializer:json;type:text;not null" json:"delivery"`
	CreatedAt    time.Time    `json:"createdAt"`
	UpdatedAt    time.Time    `json:"updatedAt"`
}
