package profiles

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
	"time"
	"unsafe"
)

func TestBuildManagerCopiesBorrowedProfileID(t *testing.T) {
	service := newTestService(t)
	profile, err := service.Create(context.Background(), ProfileRequest{
		Name: "Fiber buffer", Slug: "fiber-buffer", Loader: "fabric", GameVersion: "1.21.1",
	})
	if err != nil {
		t.Fatal(err)
	}
	writeTestFile(t, filepath.Join(service.storageRoot, profile.Slug, "files", "mods", "a.jar"), "data")

	manager := NewBuildManager(service, nil)
	manager.worker <- struct{}{}
	defer func() { <-manager.worker }()
	// Fiber/fasthttp возвращает Params как borrowed string из переиспользуемого
	// request-буфера. После ответа следующий запрос меняет его backing bytes.
	buffer := []byte(profile.ID)
	borrowedID := unsafe.String(unsafe.SliceData(buffer), len(buffer))
	started, created, err := manager.Start(context.Background(), borrowedID)
	if err != nil || !created {
		t.Fatalf("Start() created=%v err=%v", created, err)
	}
	copy(buffer, strings.Repeat("x", len(buffer)))

	restored, ok := manager.Snapshot(profile.ID)
	if !ok || restored.ID != started.ID || restored.ProfileID != profile.ID {
		t.Fatalf("borrowed profile id escaped into background state: ok=%v snapshot=%+v", ok, restored)
	}
}

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
