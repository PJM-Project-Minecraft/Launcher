package anticheat

import (
	"context"
	"errors"
	"testing"
)

type failingEntropy struct{}

func (failingEntropy) Read([]byte) (int, error) { return 0, errors.New("entropy unavailable") }

func TestInitHandshakeFailsClosedWhenEntropyUnavailable(t *testing.T) {
	svc := NewService(newTestDB(t), "secret", false, nil, "")
	svc.entropy = failingEntropy{}

	result, err := svc.InitHandshake(context.Background(), "uuid-entropy", "Liko", "", nil)
	if err == nil {
		t.Fatal("entropy failure must abort handshake")
	}
	if result.Allowed || result.LaunchToken != "" || result.Nonce != "" || result.CaptureSecret != "" {
		t.Fatalf("entropy failure exposed partial launch credentials: %+v", result)
	}
}
