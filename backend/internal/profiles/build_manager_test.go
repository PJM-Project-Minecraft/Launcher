package profiles

import (
	"context"
	"path/filepath"
	"testing"
	"time"
)

func TestBuildManagerPublishesProgressAndDeduplicatesActiveJob(t *testing.T) {
	service := newTestService(t)
	profile, err := service.Create(context.Background(), ProfileRequest{
		Name: "Manager", Slug: "manager", Loader: "fabric", GameVersion: "1.21.1",
	})
	if err != nil {
		t.Fatal(err)
	}
	writeTestFile(t, filepath.Join(service.storageRoot, profile.Slug, "files", "mods", "a.jar"), "data")

	published := make(chan struct{}, 1)
	manager := NewBuildManager(service, func() { published <- struct{}{} })
	_, events := manager.Subscribe()
	// Удерживаем единственный worker, чтобы второй Start гарантированно увидел
	// первое задание в queued, а не зависел от скорости временного диска.
	manager.worker <- struct{}{}

	first, started, err := manager.Start(context.Background(), profile.ID)
	if err != nil || !started {
		t.Fatalf("first Start() started=%v err=%v", started, err)
	}
	second, started, err := manager.Start(context.Background(), profile.ID)
	if err != nil || started || second.ID != first.ID {
		t.Fatalf("duplicate Start() = (%s, %v, %v), want same inactive start", second.ID, started, err)
	}
	<-manager.worker

	deadline := time.After(5 * time.Second)
	sawRunning := false
	for {
		select {
		case event := <-events:
			if event.Status == BuildRunning {
				sawRunning = true
			}
			if event.Status == BuildSucceeded {
				if !sawRunning || event.Progress != 1 || event.Result == nil || len(event.Logs) < 3 {
					t.Fatalf("incomplete build event: %+v", event)
				}
				select {
				case <-published:
				case <-time.After(time.Second):
					t.Fatal("publish callback was not called")
				}
				return
			}
		case <-deadline:
			t.Fatal("timed out waiting for manifest build")
		}
	}
}

func TestSocketTicketIsSingleUse(t *testing.T) {
	tickets := NewSocketTickets(time.Minute)
	ticket, _, err := tickets.Issue()
	if err != nil {
		t.Fatal(err)
	}
	if err := tickets.Consume(ticket); err != nil {
		t.Fatalf("first Consume() error = %v", err)
	}
	if err := tickets.Consume(ticket); err == nil {
		t.Fatal("second Consume() unexpectedly succeeded")
	}
}
