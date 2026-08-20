package models

type GameFile struct {
	ID         string `gorm:"type:uuid;primaryKey" json:"id"`
	ProfileID  string `gorm:"type:uuid;index;not null" json:"profileId"`
	Name       string `gorm:"size:255;not null" json:"name"`
	Path       string `gorm:"size:512;not null" json:"path"`
	URL        string `gorm:"not null" json:"url"`
	HashSHA256 string `gorm:"size:64;not null" json:"hashSha256"`
	Size       int64  `gorm:"not null" json:"size"`
	FileType   string `gorm:"size:16;not null;default:mod" json:"fileType"`
	Executable bool   `gorm:"not null;default:false" json:"executable"`
	// SourceModTimeNS — дешёвый fingerprint staging-файла. Он позволяет не читать
	// гигабайты неизменённых данных при каждой повторной публикации.
	SourceModTimeNS    int64 `gorm:"not null;default:0" json:"-"`
	SourceChangeTimeNS int64 `gorm:"not null;default:0" json:"-"`
}
