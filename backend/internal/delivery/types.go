package delivery

import "time"

const SchemaVersion = 2

type ChunkRef struct {
	SHA256 string `json:"sha256"`
	Size   int64  `json:"size"`
}

type ReleaseFile struct {
	Path       string     `json:"path"`
	Size       int64      `json:"size"`
	SHA256     string     `json:"sha256"`
	Executable bool       `json:"executable"`
	Chunks     []ChunkRef `json:"chunks"`
}

type ProfileConfig struct {
	ID                   string   `json:"id"`
	Name                 string   `json:"name"`
	GameVersion          string   `json:"gameVersion"`
	Loader               string   `json:"loader"`
	LoaderVersion        string   `json:"loaderVersion"`
	JavaVersion          int      `json:"javaVersion"`
	JVMArgs              string   `json:"jvmArgs"`
	JavaPathWindows      string   `json:"javaPathWindows"`
	JavaPathLinux        string   `json:"javaPathLinux"`
	JavaPathMacOS        string   `json:"javaPathMacos"`
	LaunchCommandWindows string   `json:"launchCommandWindows"`
	LaunchCommandLinux   string   `json:"launchCommandLinux"`
	LaunchCommandMacOS   string   `json:"launchCommandMacos"`
	PreservePaths        []string `json:"preservePaths"`
}

type ProfileManifest struct {
	SchemaVersion int           `json:"schemaVersion"`
	Kind          string        `json:"kind"`
	ReleaseID     string        `json:"releaseId"`
	Sequence      int           `json:"sequence"`
	CreatedAt     time.Time     `json:"createdAt"`
	Profile       ProfileConfig `json:"profile"`
	Files         []ReleaseFile `json:"files"`
	FileCount     int           `json:"fileCount"`
	TotalSize     int64         `json:"totalSize"`
}

type ProfileSummary struct {
	ID                string     `json:"id"`
	Name              string     `json:"name"`
	Description       string     `json:"description"`
	GameVersion       string     `json:"gameVersion"`
	Loader            string     `json:"loader"`
	IconURL           string     `json:"iconUrl"`
	ActiveReleaseID   string     `json:"activeReleaseId"`
	ManifestSHA256    string     `json:"manifestSha256"`
	ManifestSignature string     `json:"manifestSignature,omitempty"`
	ReleaseCreatedAt  *time.Time `json:"releaseCreatedAt,omitempty"`
	FileCount         int        `json:"fileCount"`
	TotalSize         int64      `json:"totalSize"`
	IsActive          bool       `json:"isActive"`
	DeliveryBaseURL   string     `json:"deliveryBaseUrl,omitempty"`
}

type LauncherManifest struct {
	SchemaVersion     int         `json:"schemaVersion"`
	Kind              string      `json:"kind"`
	ReleaseID         string      `json:"releaseId"`
	Version           string      `json:"version"`
	Platform          string      `json:"platform"`
	Changelog         string      `json:"changelog"`
	ArtifactSignature string      `json:"artifactSignature"`
	Artifact          ReleaseFile `json:"artifact"`
	CreatedAt         time.Time   `json:"createdAt"`
	DownloadURL       string      `json:"downloadUrl"`
}

type LauncherSnapshot struct {
	Descriptor []byte
	SHA256     string
	Signature  string
	Mandatory  bool
}
