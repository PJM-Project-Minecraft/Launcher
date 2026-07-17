package repo_test

import (
	"context"
	"testing"

	"launcher-backend/internal/models"
	"launcher-backend/internal/repo"
)

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
