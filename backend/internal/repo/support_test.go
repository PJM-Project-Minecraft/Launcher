package repo_test

import (
	"context"
	"testing"
	"time"

	"launcher-backend/internal/models"
	"launcher-backend/internal/repo"
)

// TestSupportCooldownAndBlock — КД между сообщениями и временная блокировка обращений.
func TestSupportCooldownAndBlock(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	const uid, chat = "bb000000-0000-0000-0000-0000000000bb", int64(777)
	if err := db.Create(&models.User{ID: uid, Login: "CooldownProbe", ProviderUUID: uid}).Error; err != nil {
		t.Fatalf("create user: %v", err)
	}

	if until, wait, err := repo.SupportDenied(ctx, db, uid); err != nil || until != nil || wait != 0 {
		t.Fatalf("до тикетов писать можно: until=%v wait=%v err=%v", until, wait, err)
	}

	id, _, err := repo.CreateOrAppendSupport(ctx, db, uid, chat, "первый вопрос")
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	_, wait, err := repo.SupportDenied(ctx, db, uid)
	if err != nil || wait <= 0 {
		t.Fatalf("сразу после сообщения должен быть КД: wait=%v err=%v", wait, err)
	}

	// Отматываем активность тикета назад — КД должен истечь.
	old := time.Now().UTC().Add(-repo.SupportCooldown - time.Minute)
	if err := db.Exec("UPDATE bot_support_tickets SET updated_at = ? WHERE id = ?", old, id).Error; err != nil {
		t.Fatalf("backdate: %v", err)
	}
	if _, wait, err := repo.SupportDenied(ctx, db, uid); err != nil || wait != 0 {
		t.Fatalf("КД должен истечь: wait=%v err=%v", wait, err)
	}

	// Блокировка перевешивает КД и снимается по истечении срока.
	future := time.Now().UTC().Add(time.Hour)
	if err := repo.SetSupportBlock(ctx, db, uid, &future); err != nil {
		t.Fatalf("block: %v", err)
	}
	if until, _, err := repo.SupportDenied(ctx, db, uid); err != nil || until == nil {
		t.Fatalf("блокировка не сработала: until=%v err=%v", until, err)
	}
	past := time.Now().UTC().Add(-time.Minute)
	if err := repo.SetSupportBlock(ctx, db, uid, &past); err != nil {
		t.Fatalf("expire: %v", err)
	}
	if until, _, err := repo.SupportDenied(ctx, db, uid); err != nil || until != nil {
		t.Fatalf("истёкшая блокировка не должна держать: until=%v err=%v", until, err)
	}
	if err := repo.SetSupportBlock(ctx, db, uid, nil); err != nil {
		t.Fatalf("unblock: %v", err)
	}
	if until, _, err := repo.SupportDenied(ctx, db, uid); err != nil || until != nil {
		t.Fatalf("после снятия блокировки не должно быть: until=%v err=%v", until, err)
	}
}

// TestSupportTicketDedupAndClose — открытый тикет один на игрока: follow-up
// дописывает LastMessage, не плодит новый; после закрытия следующее сообщение
// заводит новый тикет.
func TestSupportTicketDedupAndClose(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	const uid, chat = "user-uuid", int64(555)

	id1, created, err := repo.CreateOrAppendSupport(ctx, db, uid, chat, "не запускается")
	if err != nil || !created {
		t.Fatalf("first: created=%v err=%v", created, err)
	}

	id2, created, err := repo.CreateOrAppendSupport(ctx, db, uid, chat, "всё ещё падает")
	if err != nil {
		t.Fatalf("append: %v", err)
	}
	if created || id2 != id1 {
		t.Fatalf("follow-up должен дописать тот же тикет: created=%v id2=%d id1=%d", created, id2, id1)
	}
	if tk, _ := repo.GetSupportTicket(ctx, db, id1); tk == nil || tk.LastMessage != "всё ещё падает" {
		t.Fatalf("LastMessage не обновился: %+v", tk)
	}

	ok, err := repo.CloseSupportTicket(ctx, db, id1)
	if err != nil || !ok {
		t.Fatalf("close: ok=%v err=%v", ok, err)
	}
	if ok, _ := repo.CloseSupportTicket(ctx, db, id1); ok {
		t.Fatalf("повторное закрытие должно вернуть false")
	}

	id3, created, err := repo.CreateOrAppendSupport(ctx, db, uid, chat, "новый вопрос")
	if err != nil || !created || id3 == id1 {
		t.Fatalf("после закрытия нужен новый тикет: created=%v id3=%d id1=%d err=%v", created, id3, id1, err)
	}
	if tk, _ := repo.GetSupportTicket(ctx, db, id3); tk == nil || tk.Status != models.SupportOpen {
		t.Fatalf("новый тикет должен быть open: %+v", tk)
	}
}
