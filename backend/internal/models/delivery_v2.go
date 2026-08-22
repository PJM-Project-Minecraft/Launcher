package models

import "time"

type DeliveryBlob struct {
	HashSHA256 string    `gorm:"size:64;primaryKey" json:"hashSha256"`
	Size       int64     `gorm:"not null" json:"size"`
	CreatedAt  time.Time `json:"createdAt"`
}
type ProfileRelease struct {
	ID                string               `gorm:"type:uuid;primaryKey" json:"id"`
	ProfileID         string               `gorm:"type:uuid;index;not null;uniqueIndex:ux_profile_release_sequence" json:"profileId"`
	Sequence          int                  `gorm:"not null;uniqueIndex:ux_profile_release_sequence" json:"sequence"`
	ManifestSHA256    string               `gorm:"size:64;not null" json:"manifestSha256"`
	ManifestSignature string               `gorm:"size:128" json:"manifestSignature,omitempty"`
	FileCount         int                  `gorm:"not null" json:"fileCount"`
	TotalSize         int64                `gorm:"not null" json:"totalSize"`
	IsActive          bool                 `gorm:"index;not null;default:false" json:"isActive"`
	Files             []ProfileReleaseFile `gorm:"foreignKey:ReleaseID;constraint:OnDelete:CASCADE" json:"-"`
	CreatedAt         time.Time            `json:"createdAt"`
}
type ProfileReleaseFile struct {
	ID         string                    `gorm:"type:uuid;primaryKey" json:"id"`
	ReleaseID  string                    `gorm:"type:uuid;index;not null;uniqueIndex:ux_profile_release_path" json:"releaseId"`
	Path       string                    `gorm:"not null;uniqueIndex:ux_profile_release_path" json:"path"`
	HashSHA256 string                    `gorm:"size:64;not null" json:"sha256"`
	Size       int64                     `gorm:"not null" json:"size"`
	Executable bool                      `gorm:"not null;default:false" json:"executable"`
	Chunks     []ProfileReleaseFileChunk `gorm:"foreignKey:FileID;constraint:OnDelete:CASCADE" json:"chunks"`
}
type ProfileReleaseFileChunk struct {
	ID         string `gorm:"type:uuid;primaryKey" json:"-"`
	FileID     string `gorm:"type:uuid;index;not null;uniqueIndex:ux_profile_file_chunk_ordinal" json:"-"`
	Ordinal    int    `gorm:"not null;uniqueIndex:ux_profile_file_chunk_ordinal" json:"ordinal"`
	HashSHA256 string `gorm:"size:64;index;not null" json:"sha256"`
	Size       int64  `gorm:"not null" json:"size"`
}
type LauncherDeliveryArtifact struct {
	ID                  string                          `gorm:"type:uuid;primaryKey" json:"id"`
	ReleaseID           string                          `gorm:"type:uuid;index;not null;uniqueIndex:ux_launcher_delivery_platform" json:"releaseId"`
	Platform            string                          `gorm:"size:32;index;not null;uniqueIndex:ux_launcher_delivery_platform" json:"platform"`
	HashSHA256          string                          `gorm:"size:64;not null" json:"sha256"`
	Size                int64                           `gorm:"not null" json:"size"`
	Executable          bool                            `gorm:"not null;default:true" json:"executable"`
	DescriptorJSON      string                          `gorm:"type:text" json:"-"`
	DescriptorSHA256    string                          `gorm:"size:64" json:"descriptorSha256"`
	DescriptorSignature string                          `gorm:"size:128" json:"descriptorSignature"`
	Chunks              []LauncherDeliveryArtifactChunk `gorm:"foreignKey:ArtifactID;constraint:OnDelete:CASCADE" json:"chunks"`
	CreatedAt           time.Time                       `json:"createdAt"`
}
type LauncherDeliveryArtifactChunk struct {
	ID         string `gorm:"type:uuid;primaryKey" json:"-"`
	ArtifactID string `gorm:"type:uuid;index;not null;uniqueIndex:ux_launcher_artifact_chunk_ordinal" json:"-"`
	Ordinal    int    `gorm:"not null;uniqueIndex:ux_launcher_artifact_chunk_ordinal" json:"ordinal"`
	HashSHA256 string `gorm:"size:64;index;not null" json:"sha256"`
	Size       int64  `gorm:"not null" json:"size"`
}
type DeliveryJob struct {
	ID              string     `gorm:"type:uuid;primaryKey" json:"id"`
	Kind            string     `gorm:"size:24;index;not null" json:"kind"`
	ProfileID       *string    `gorm:"type:uuid;index" json:"profileId,omitempty"`
	Generation      string     `gorm:"size:160;uniqueIndex;not null" json:"generation"`
	Status          string     `gorm:"size:24;index;not null" json:"status"`
	Phase           string     `gorm:"size:48;not null" json:"phase"`
	Message         string     `json:"message"`
	Progress        float64    `gorm:"not null;default:0" json:"progress"`
	Error           string     `json:"error,omitempty"`
	ReleaseID       *string    `gorm:"type:uuid" json:"releaseId,omitempty"`
	SourceReleaseID *string    `gorm:"type:uuid;index" json:"sourceReleaseId,omitempty"`
	CreatedAt       time.Time  `json:"createdAt"`
	StartedAt       *time.Time `json:"startedAt,omitempty"`
	EndedAt         *time.Time `json:"endedAt,omitempty"`
	UpdatedAt       time.Time  `json:"updatedAt"`
}
