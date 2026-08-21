package models

import "time"

// GameKitCatalog — публичный снимок одного роль-кита, авторитетно заменяемый игровым сервером.
type GameKitCatalog struct {
	RoleID    string          `gorm:"size:64;primaryKey" json:"roleId"`
	Snapshot  GameKitSnapshot `gorm:"serializer:json;type:text;not null" json:"snapshot"`
	UpdatedAt time.Time       `json:"updatedAt"`
}

type GameKitSnapshot struct {
	RoleID      string        `json:"roleId"`
	DisplayName string        `json:"displayName"`
	Primary     []KitWeapon   `json:"primary"`
	Sidearm     []KitWeapon   `json:"sidearm"`
	Gear        []KitGearSlot `json:"gear"`
	Fixed       []KitFixed    `json:"fixed"`
}

type KitWeapon struct {
	ID           string              `json:"id"`
	GunID        string              `json:"gunId"`
	Name         string              `json:"name"`
	MinRank      string              `json:"minRank"`
	AllowedTeams []string            `json:"allowedTeams"`
	Restricted   bool                `json:"restricted"`
	Magazines    int                 `json:"magazines"`
	AmmoID       string              `json:"ammoId"`
	Attachments  []KitAttachmentSlot `json:"attachments"`
}

type KitAttachmentSlot struct {
	Slot    string      `json:"slot"`
	Options []KitOption `json:"options"`
}

type KitGearSlot struct {
	Slot    string        `json:"slot"`
	Options []KitGearItem `json:"options"`
}

type KitGearItem struct {
	ID           string      `json:"id"`
	ItemID       string      `json:"itemId"`
	Name         string      `json:"name"`
	MinRank      string      `json:"minRank"`
	AllowedTeams []string    `json:"allowedTeams"`
	Restricted   bool        `json:"restricted"`
	AmmoCount    int         `json:"ammoCount"`
	AmmoOptions  []KitOption `json:"ammoOptions"`
}

type KitOption struct {
	ID         string `json:"id"`
	Name       string `json:"name"`
	MinRank    string `json:"minRank"`
	Restricted bool   `json:"restricted"`
}

type KitFixed struct {
	ID     string `json:"id"`
	ItemID string `json:"itemId"`
	Name   string `json:"name"`
	Count  int    `json:"count"`
}
