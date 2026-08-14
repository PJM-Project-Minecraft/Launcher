package models

import "time"

const (
	OrderStatusPaid   = "paid"
	OrderStatusIssued = "issued"
)

// OrderItem — неизменяемый снимок товара и цены на момент оплаты.
// Цена и Total хранятся в копейках, чтобы денежный путь не использовал float.
type OrderItem struct {
	ID    int64  `json:"id"`
	Name  string `json:"name"`
	Qty   int    `json:"qty"`
	Price int64  `json:"price"`
}

// Order — подтверждённый через YooKassa заказ сайта.
// OrderID является публичным идемпотентным ключом, YooKassaID — id платежа.
type Order struct {
	ID         uint        `gorm:"primaryKey" json:"id"`
	OrderID    string      `gorm:"type:uuid;uniqueIndex;not null" json:"orderId"`
	Nick       string      `gorm:"size:16;index;not null" json:"nick"`
	Items      []OrderItem `gorm:"serializer:json;type:text;not null" json:"items"`
	Total      int64       `gorm:"not null" json:"total"`
	YooKassaID string      `gorm:"size:128;uniqueIndex;not null" json:"yookassaId"`
	Status     string      `gorm:"size:16;index;not null" json:"status"`
	PaidAt     time.Time   `gorm:"index;not null" json:"paidAt"`
	IssuedAt   *time.Time  `json:"issuedAt,omitempty"`
	CreatedAt  time.Time   `gorm:"index" json:"createdAt"`
}
