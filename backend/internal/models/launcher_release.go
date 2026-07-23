package models

import "time"

// LauncherRelease — версия десктоп-лаунчера, заливается через дашборд.
// Mandatory: клиенты ниже этой версии не получают launch-token (форс-апдейт).
// IsActive=false снимает релиз с раздачи (откат публикации).
type LauncherRelease struct {
	ID        string                `gorm:"type:uuid;primaryKey" json:"id"`
	Version   string                `gorm:"size:32;uniqueIndex;not null" json:"version"`
	Changelog string                `json:"changelog"`
	Mandatory bool                  `gorm:"not null;default:false" json:"mandatory"`
	IsActive  bool                  `gorm:"not null;default:true" json:"isActive"`
	Files     []LauncherReleaseFile `gorm:"foreignKey:ReleaseID" json:"files"`
	CreatedAt time.Time             `json:"createdAt"`
	UpdatedAt time.Time             `json:"updatedAt"`
}

// LauncherReleaseFile — бинарник релиза под конкретную платформу.
// Лежит на диске в storage/releases/<version>/<platform>/<FileName>.
type LauncherReleaseFile struct {
	ID         string `gorm:"type:uuid;primaryKey" json:"id"`
	ReleaseID  string `gorm:"type:uuid;index;not null" json:"releaseId"`
	Platform   string `gorm:"size:32;not null" json:"platform"`
	FileName   string `gorm:"size:255;not null" json:"fileName"`
	HashSHA256 string `gorm:"size:64;not null" json:"hashSha256"`
	Size       int64  `gorm:"not null" json:"size"`
	// SignatureEd25519 — hex-подпись Ed25519 над байтами бинарника, сделанная ОФФЛАЙН
	// приватным ключом релиз-бокса (в БД/на сервере его нет). Лаунчер сверяет её со
	// вшитым публичным ключом → скомпрометированный сервер/зеркало не подсунут чужой
	// бинарник (SHA один недостаточен: приходит тем же каналом, что и файл). Пусто —
	// релиз без подписи (лаунчер без вшитого ключа примет по SHA, с ключом — отвергнет).
	SignatureEd25519 string `gorm:"size:128" json:"signatureEd25519,omitempty"`
}
