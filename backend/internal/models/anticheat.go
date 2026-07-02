package models

import "time"

// Detection — зафиксированное античитом обнаружение (от лаунчера или агента).
// Привязано к игроку (UserUUID — нормализованный Yggdrasil-UUID) и по возможности
// к железу (HwidHash) и игровой сессии (SessionID — accessToken-хэш).
type Detection struct {
	ID        string `gorm:"type:uuid;primaryKey" json:"id"`
	UserUUID  string `gorm:"size:64;index;not null" json:"userUuid"`
	Login     string `gorm:"size:64;index" json:"login"`
	HwidHash  string `gorm:"size:64;index" json:"hwidHash"`
	SessionID string `gorm:"size:64;index" json:"sessionId"`
	Source    string `gorm:"size:16;not null;default:launcher" json:"source"` // launcher|java|native
	Type      string `gorm:"size:32;not null" json:"type"`                    // process|class|jar|file|attach|debugger|tamper
	Signature string `gorm:"size:255" json:"signature"`                       // что именно сработало
	Severity  int    `gorm:"not null;default:1" json:"severity"`              // 1..10
	// Confidence — серверная уверенность в детекте: hard (кандидат на авто-бан/кик) или
	// soft (эвристика, возможен ложняк — только в review-очередь). См. detectionConfidence.
	Confidence string `gorm:"size:8;index" json:"confidence"` // hard|soft
	Raw        string `gorm:"type:text" json:"raw"`           // JSON с деталями
	// Review-очередь: статус разбора оператором и кто/когда его проставил.
	Status     string     `gorm:"size:16;not null;default:new;index" json:"status"` // new|reviewed|confirmed|dismissed
	ReviewedBy string     `gorm:"size:64" json:"reviewedBy"`
	ReviewedAt *time.Time `json:"reviewedAt"`
	CreatedAt  time.Time  `gorm:"index" json:"createdAt"`
}

// Hwid — известный аппаратный отпечаток и история его появления. Hash — агрегатный
// солёный SHA-256 (совместимость со старыми банами); компонентные хеши — для fuzzy-матча
// (смена нестабильного MAC не должна обходить бан, а одиночная коллизия — не банить).
type Hwid struct {
	Hash          string    `gorm:"size:64;primaryKey" json:"hash"`
	FirstUserUUID string    `gorm:"size:64;index" json:"firstUserUuid"`
	FirstLogin    string    `gorm:"size:64" json:"firstLogin"`
	SeenCount     int64     `gorm:"not null;default:0" json:"seenCount"`
	FirstSeen     time.Time `json:"firstSeen"`
	LastSeen      time.Time `json:"lastSeen"`
	// Раздельные солёные хеши компонентов (пустые — старый клиент их не прислал).
	MachineIDHash string `gorm:"size:64;index" json:"machineIdHash"`
	BoardUUIDHash string `gorm:"size:64;index" json:"boardUuidHash"`
	MacHashes     string `gorm:"type:text" json:"macHashes"` // JSON-массив солёных хешей MAC
}

// HwidBan — аппаратный бан. Активен, если ExpiresAt == nil (перманентно) или в будущем.
type HwidBan struct {
	ID        string     `gorm:"type:uuid;primaryKey" json:"id"`
	HwidHash  string     `gorm:"size:64;uniqueIndex;not null" json:"hwidHash"`
	Reason    string     `gorm:"size:255" json:"reason"`
	BannedBy  string     `gorm:"size:64" json:"bannedBy"` // login админа
	CreatedAt time.Time  `json:"createdAt"`
	ExpiresAt *time.Time `json:"expiresAt"`
}

// AccountBan — бан аккаунта по нормализованному UUID.
type AccountBan struct {
	ID        string     `gorm:"type:uuid;primaryKey" json:"id"`
	UserUUID  string     `gorm:"size:64;uniqueIndex;not null" json:"userUuid"`
	Login     string     `gorm:"size:64;index" json:"login"`
	Reason    string     `gorm:"size:255" json:"reason"`
	BannedBy  string     `gorm:"size:64" json:"bannedBy"`
	CreatedAt time.Time  `json:"createdAt"`
	ExpiresAt *time.Time `json:"expiresAt"`
}

// Screenshot — скриншот экрана игрока по запросу админа (античит). Загружается
// лаунчером игрока по pending-запросу, хранится в storage/screenshots и доступен
// для повторного просмотра в дашборде. AutoMigrate создаёт таблицу; файлы — в ФС.
type Screenshot struct {
	ID        string    `gorm:"type:uuid;primaryKey" json:"id"`
	UserUUID  string    `gorm:"size:64;index;not null" json:"userUuid"`
	Login     string    `gorm:"size:64;index" json:"login"`
	Nonce     string    `gorm:"size:64;index" json:"nonce"` // игровая сессия на момент запроса
	// Status: pending (запрос создан, ждёт лаунчер) | capturing (лаунчер взял в работу)
	// | done (скриншот загружен) | failed (ошибка/таймаут).
	Status    string    `gorm:"size:16;not null;default:pending;index" json:"status"`
	RequestedBy string  `gorm:"size:64" json:"requestedBy"` // login админа
	FileName  string    `gorm:"size:255" json:"fileName"`    // имя файла в storage
	Width     int       `json:"width"`
	Height    int       `json:"height"`
	Size      int64     `json:"size"` // байты JPEG
	Error     string    `gorm:"size:255" json:"error"` // причина, если failed
	CreatedAt time.Time `gorm:"index" json:"createdAt"`
	CapturedAt *time.Time `json:"capturedAt,omitempty"` // когда лаунчер загрузил
}

// CheatSignature — запись блэклиста, по которой лаунчер и агенты ищут читы.
// Kind задаёт, что сопоставляется; способ матча — MatchType (по Pattern или HashHex).
type CheatSignature struct {
	ID      string `gorm:"type:uuid;primaryKey" json:"id"`
	Kind    string `gorm:"size:16;not null" json:"kind"` // process|class|jar|file
	Pattern string `gorm:"size:255" json:"pattern"`      // имя/подстрока (lowercase)
	// MatchType — как сопоставлять Pattern с сигналом детекта: substring (дефолт —
	// обратная совместимость со старым поведением), exact (полное равенство), word
	// (по границам слова), regex (RE2), hash (по HashHex вместо Pattern). Точные типы
	// (exact|word|regex|hash) считаются hard-детектом, substring — soft (анти-FP).
	MatchType string    `gorm:"size:16;not null;default:substring" json:"matchType"`
	HashHex   string    `gorm:"size:64;index" json:"hashHex"` // SHA-256, если матч по хэшу
	Severity  int       `gorm:"not null;default:5" json:"severity"`
	Note      string    `gorm:"size:255" json:"note"`
	Enabled   bool      `gorm:"not null;default:true" json:"enabled"`
	CreatedAt time.Time `json:"createdAt"`
	UpdatedAt time.Time `json:"updatedAt"`
}
