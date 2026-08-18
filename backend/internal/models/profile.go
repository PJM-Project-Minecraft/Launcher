package models

import "time"

type Profile struct {
	ID                   string     `gorm:"type:uuid;primaryKey" json:"id"`
	Name                 string     `gorm:"size:64;not null" json:"name"`
	Slug                 string     `gorm:"size:80;uniqueIndex" json:"slug"`
	Description          string     `json:"description"`
	Loader               string     `gorm:"size:16;not null" json:"loader"`
	GameVersion          string     `gorm:"size:16;not null" json:"gameVersion"`
	LoaderVersion        string     `gorm:"size:32" json:"loaderVersion"`
	JavaVersion          int        `gorm:"not null;default:17" json:"javaVersion"`
	JVMArgs              string     `gorm:"column:jvm_args" json:"jvmArgs"`
	IconURL              string     `json:"iconUrl"`
	JavaPathWindows      string     `gorm:"size:512" json:"javaPathWindows"`
	JavaPathLinux        string     `gorm:"size:512" json:"javaPathLinux"`
	JavaPathMacOS        string     `gorm:"size:512" json:"javaPathMacos"`
	LaunchCommandWindows string     `json:"launchCommandWindows"`
	LaunchCommandLinux   string     `json:"launchCommandLinux"`
	LaunchCommandMacOS   string     `json:"launchCommandMacos"`
	PreservePaths        []string   `gorm:"serializer:json;column:preserve_paths" json:"preservePaths"`
	ManifestVersion      int        `gorm:"not null;default:0" json:"manifestVersion"`
	ManifestUpdatedAt    *time.Time `json:"manifestUpdatedAt"`
	BundleHashSHA256     string     `gorm:"size:64" json:"bundleHashSha256"`
	BundleSize           int64      `gorm:"not null;default:0" json:"bundleSize"`
	IsActive             bool       `gorm:"not null;default:true" json:"isActive"`
	CreatedAt            time.Time  `json:"createdAt"`
	UpdatedAt            time.Time  `json:"updatedAt"`
}
