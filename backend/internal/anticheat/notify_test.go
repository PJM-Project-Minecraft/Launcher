package anticheat

import "testing"

// Тип/сигнатуру детекта задаёт клиент, а лимитер детектов — per-UUID: с нескольких
// аккаунтов канал алертов забивался мусором и настоящий детект терялся. Проверяем
// глобальный потолок и то, что число подавленных доезжает до оператора.
func TestNotifierRateLimitsAlerts(t *testing.T) {
	n := NewTelegramNotifier("token", "chat")
	for i := 0; i < alertsPerMinute; i++ {
		if ok, _ := n.allow(); !ok {
			t.Fatalf("алерт %d в пределах потолка должен проходить", i)
		}
	}
	if ok, _ := n.allow(); ok {
		t.Fatal("сверх потолка алерты обязаны подавляться")
	}
	if ok, _ := n.allow(); ok {
		t.Fatal("подавление должно держаться до конца окна")
	}

	// Окно сдвинулось: очередной алерт проходит и приносит счётчик подавленных.
	n.sentAt = nil
	ok, suppressed := n.allow()
	if !ok {
		t.Fatal("после окна алерты должны возобновляться")
	}
	if suppressed != 2 {
		t.Fatalf("оператор должен узнать о 2 подавленных, получено %d", suppressed)
	}
}
