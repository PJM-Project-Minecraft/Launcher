package anticheat

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"testing"
)

func TestManifest(t *testing.T) {
	dir := t.TempDir()
	agent := filepath.Join(dir, "agent.jar")
	content := []byte("fake-agent-bytes")
	if err := os.WriteFile(agent, content, 0o644); err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(content)
	want := hex.EncodeToString(sum[:])

	svc := NewService(newTestDB(t), "secret", false, nil, agent)
	m := svc.Manifest()
	if m.AgentSha256 != want {
		t.Fatalf("SHA agent.jar не совпал: got %s want %s", m.AgentSha256, want)
	}
	// Кэш: повторный вызов даёт тот же результат.
	if svc.Manifest().AgentSha256 != want {
		t.Fatal("кэшированный SHA отличается")
	}
	// Несуществующий нативный путь → пустая строка (опускается в JSON).
	if m.Native.Linux != "" {
		t.Fatalf("ожидалась пустая строка для отсутствующего файла, got %q", m.Native.Linux)
	}

	// Изменение файла инвалидирует кэш.
	content2 := []byte("tampered-agent-bytes!!")
	if err := os.WriteFile(agent, content2, 0o644); err != nil {
		t.Fatal(err)
	}
	sum2 := sha256.Sum256(content2)
	if svc.Manifest().AgentSha256 != hex.EncodeToString(sum2[:]) {
		t.Fatal("кэш не инвалидировался после изменения файла")
	}
}

func TestConfirmRequiresAttestation(t *testing.T) {
	dir := t.TempDir()
	agent := filepath.Join(dir, "agent.jar")
	content := []byte("agent-bytes-xyz")
	if err := os.WriteFile(agent, content, 0o644); err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(content)
	agentSha := hex.EncodeToString(sum[:])

	v := &fakeVerifier{verified: map[string]bool{}}
	svc := NewService(newTestDB(t), "secret", false, v, agent)
	svc.SetRequireAttestation(true)
	ctx := context.Background()

	res, _ := svc.InitHandshake(ctx, "uuid-att", "Liko", "hwid-att", nil)
	if res.Challenge == "" {
		t.Fatal("init должен вернуть challenge")
	}

	// Пустой proof (нет challenge/native) при requireAttestation → отказ.
	if err := svc.Confirm(res.LaunchToken, ConfirmProof{}); err == nil {
		t.Fatal("confirm без валидного proof должен быть отклонён при requireAttestation")
	}
	// Неверный self-hash → отказ.
	bad := ConfirmProof{Challenge: res.Challenge, AgentSha256: "deadbeef", NativePresent: true}
	if err := svc.Confirm(res.LaunchToken, bad); err == nil {
		t.Fatal("confirm с неверным agentSha256 должен быть отклонён")
	}
	// Корректный proof → успех.
	good := ConfirmProof{Challenge: res.Challenge, AgentSha256: agentSha, NativePresent: true, ForeignAgents: false}
	if err := svc.Confirm(res.LaunchToken, good); err != nil {
		t.Fatalf("валидный proof должен пройти confirm: %v", err)
	}
	if !v.IsActiveByNonce(res.Nonce) {
		t.Fatal("после валидного confirm сессия должна быть активна")
	}
}
