package models

import "time"

const (
	DeliveryTypeNone = "none"
	DeliveryTypeRole = "role"
	DeliveryTypeItem = "item"

	DeliveryStatusPending = "pending"
	DeliveryStatusDone    = "done"
)

// DeliverySpec — неизменяемое действие, которое PJM-BaseMod применяет игроку.
// Для role используется RoleID, для item — ItemID и Count.
type DeliverySpec struct {
	Type   string `json:"type"`
	RoleID string `json:"roleId,omitempty"`
	ItemID string `json:"itemId,omitempty"`
	Count  int    `json:"count,omitempty"`
}

// Delivery — одна игровая выдача для одной позиции оплаченного заказа.
// ItemIndex делает создание идемпотентным даже если в будущем корзина сможет
// содержать две строки с одинаковым внешним id.
type Delivery struct {
	ID         uint         `gorm:"primaryKey" json:"id"`
	OrderID    string       `gorm:"type:uuid;uniqueIndex:idx_delivery_order_item_part;not null;index" json:"orderId"`
	ItemIndex  int          `gorm:"uniqueIndex:idx_delivery_order_item_part;not null" json:"itemIndex"`
	PartIndex  int          `gorm:"uniqueIndex:idx_delivery_order_item_part;not null" json:"partIndex"`
	ShopItemID int64        `gorm:"index;not null" json:"shopItemId"`
	ItemName   string       `gorm:"size:300;not null" json:"itemName"`
	Nick       string       `gorm:"size:16;index;not null" json:"nick"`
	Delivery   DeliverySpec `gorm:"serializer:json;type:text;not null" json:"delivery"`
	Status     string       `gorm:"size:16;index;not null" json:"status"`
	ClaimedAt  *time.Time   `gorm:"index" json:"claimedAt,omitempty"`
	DoneAt     *time.Time   `json:"doneAt,omitempty"`
	CreatedAt  time.Time    `gorm:"index" json:"createdAt"`
}
