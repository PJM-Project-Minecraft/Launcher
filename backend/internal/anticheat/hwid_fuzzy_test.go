package anticheat

import (
	"context"
	"testing"

	"launcher-backend/internal/models"

	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
)

// fuzzyTestDB — изолированная БД: fuzzy-матч сканирует ВСЕ баны, поэтому записи не должны
// протекать между тестами через общий cache newTestDB.
func fuzzyTestDB(t *testing.T, name string) *gorm.DB {
	t.Helper()
	db, err := gorm.Open(sqlite.Open("file:"+name+"?mode=memory&cache=shared"), &gorm.Config{})
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	if err := db.AutoMigrate(&models.Detection{}, &models.Hwid{}, &models.HwidBan{}, &models.AccountBan{}, &models.CheatSignature{}); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	return db
}

// Смена нестабильного компонента (MAC) не должна обходить HWID-бан: machine_id и
// board_uuid стабильны, fuzzy-матч их ловит.
func TestFuzzyHwidBanSurvivesMacChange(t *testing.T) {
	svc := NewService(fuzzyTestDB(t, "fuzzy_mac"), "secret", false, nil, "")
	ctx := context.Background()
	compsA := HwidComponents{MachineID: "Mhash", BoardUUID: "Bhash", Macs: []string{"mac1"}}
	if _, err := svc.InitHandshakeWithComponents(ctx, "uuidA", "A", "hwid-1", compsA, nil); err != nil {
		t.Fatalf("init1: %v", err)
	}
	if err := svc.BanHwid(ctx, "hwid-1", "cheat", "admin"); err != nil {
		t.Fatalf("ban: %v", err)
	}
	// Возвращается со сменённым MAC → другой агрегат hwid-2, те же machine+board.
	compsA2 := HwidComponents{MachineID: "Mhash", BoardUUID: "Bhash", Macs: []string{"mac2"}}
	res, err := svc.InitHandshakeWithComponents(ctx, "uuidA", "A", "hwid-2", compsA2, nil)
	if err != nil {
		t.Fatalf("init2: %v", err)
	}
	if res.Allowed {
		t.Fatal("fuzzy: смена MAC не должна обходить бан (machine+board совпадают)")
	}
}

// Один совпавший стабильный компонент НЕ банит — защита от коллизии (общий образ ОС,
// партия одинаковых ноутбуков с одним product_uuid).
func TestFuzzyHwidNoFalsePositiveOnSingleComponent(t *testing.T) {
	svc := NewService(fuzzyTestDB(t, "fuzzy_single"), "secret", false, nil, "")
	ctx := context.Background()
	banned := HwidComponents{MachineID: "M1", BoardUUID: "Bshared", Macs: nil}
	if _, err := svc.InitHandshakeWithComponents(ctx, "uuidX", "X", "hwid-x", banned, nil); err != nil {
		t.Fatalf("init: %v", err)
	}
	if err := svc.BanHwid(ctx, "hwid-x", "cheat", "admin"); err != nil {
		t.Fatalf("ban: %v", err)
	}
	// Другой игрок: совпадает только board (одинаковая партия), machine_id разный.
	other := HwidComponents{MachineID: "M2", BoardUUID: "Bshared", Macs: nil}
	res, err := svc.InitHandshakeWithComponents(ctx, "uuidY", "Y", "hwid-y", other, nil)
	if err != nil {
		t.Fatalf("init2: %v", err)
	}
	if !res.Allowed {
		t.Fatal("один совпавший компонент (board) НЕ должен банить — защита от FP")
	}
}

// Слабый отпечаток (< 2 стабильных компонентов) не участвует в fuzzy-матче.
func TestFuzzyHwidSkipsWeakFingerprint(t *testing.T) {
	svc := NewService(fuzzyTestDB(t, "fuzzy_weak"), "secret", false, nil, "")
	ctx := context.Background()
	// Бан с полным отпечатком.
	banned := HwidComponents{MachineID: "Mw", BoardUUID: "Bw", Macs: nil}
	if _, err := svc.InitHandshakeWithComponents(ctx, "uuidW", "W", "hwid-w", banned, nil); err != nil {
		t.Fatalf("init: %v", err)
	}
	if err := svc.BanHwid(ctx, "hwid-w", "cheat", "admin"); err != nil {
		t.Fatalf("ban: %v", err)
	}
	// Игрок с только одним компонентом (machine_id пуст) — fuzzy не применяется.
	weak := HwidComponents{MachineID: "", BoardUUID: "Bw", Macs: nil}
	res, err := svc.InitHandshakeWithComponents(ctx, "uuidV", "V", "hwid-v", weak, nil)
	if err != nil {
		t.Fatalf("init2: %v", err)
	}
	if !res.Allowed {
		t.Fatal("слабый отпечаток (< 2 стабильных) не должен ловиться fuzzy")
	}
}

// Точный HWID-бан по агрегату работает и для клиента без компонентов (совместимость).
func TestExactHwidBanStillWorks(t *testing.T) {
	svc := NewService(fuzzyTestDB(t, "fuzzy_exact"), "secret", false, nil, "")
	ctx := context.Background()
	if err := svc.BanHwid(ctx, "hwid-exact", "cheat", "admin"); err != nil {
		t.Fatalf("ban: %v", err)
	}
	res, err := svc.InitHandshakeWithComponents(ctx, "uuidZ", "Z", "hwid-exact", HwidComponents{}, nil)
	if err != nil {
		t.Fatalf("init: %v", err)
	}
	if res.Allowed {
		t.Fatal("точный HWID-бан должен работать (обратная совместимость)")
	}
}
