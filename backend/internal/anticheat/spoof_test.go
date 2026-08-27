package anticheat

import (
	"context"
	"strings"
	"testing"
	"time"

	"launcher-backend/internal/models"
)

type identityVerifier struct {
	*fakeVerifier
	owners map[string][2]string
}

func (f *identityVerifier) SessionOwnerByNonce(nonce string) (string, string, bool) {
	owner, ok := f.owners[nonce]
	return owner[0], owner[1], ok
}

func TestSignedClaimsCannotBorrowAnotherPlayersNonce(t *testing.T) {
	nonce := "attacker-live-nonce"
	verifier := &identityVerifier{
		fakeVerifier: &fakeVerifier{verified: map[string]bool{}},
		owners:       map[string][2]string{nonce: {"attacker-uuid", "Attacker"}},
	}
	svc := NewService(newTestDB(t), "secret", false, verifier, "")
	token, err := svc.signer.Sign(LaunchClaims{
		UUID: "victim-uuid", Login: "Victim", Nonce: nonce,
		Expires: time.Now().Add(time.Minute).Unix(),
	})
	if err != nil {
		t.Fatalf("sign: %v", err)
	}
	if err := svc.Confirm(token, ConfirmProof{}); err == nil {
		t.Fatal("confirm accepted signed victim claims over attacker nonce")
	}
	if verifier.verified[nonce] {
		t.Fatal("mismatched claims reached verification side effect")
	}
	if _, err := svc.VerifySessionToken(token); err == nil {
		t.Fatal("session endpoint accepted signed victim claims over attacker nonce")
	}
}

// byUser отбирает детекты одного игрока: тестовая БД в пакете общая (file::memory:
// cache=shared), поэтому глобальные списки содержат записи соседних тестов.
func byUser(rows []models.Detection, uuid string) []models.Detection {
	var out []models.Detection
	for _, r := range rows {
		if r.UserUUID == uuid {
			out = append(out, r)
		}
	}
	return out
}

// Регрессия: игрок с валидным JWT слал в handshake/init произвольные детекты
// ("type":"1","signature":"1", hwidHash:"1") — каждый уходил в Telegram как severity 5
// и вставал в review-очередь. Несовпавший детект должен сохраняться как unmatched:
// без алерта, вне очереди и вне статистики сигнатур.
func TestUnmatchedDetectionIsNotAlertedOrQueued(t *testing.T) {
	svc := NewService(newTestDB(t), "secret", false, nil, "")
	ctx := context.Background()

	res, err := svc.InitHandshake(ctx, "uuid-spoof", "svocraft", "", []DetectionInput{
		{Type: "1", Signature: "spoof-junk-a"},
		{Type: "signature", Signature: "spoof-junk-b"}, // тип не системный, сигнатуры в блэклисте нет
		{Type: "inject", Signature: "foreign-agent"},   // системный тип — настоящий детект
	})
	if err != nil || !res.Allowed {
		t.Fatalf("init: %v %+v", err, res)
	}

	all, err := svc.ListDetections(ctx, 500, DetectionFilter{})
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	queued := byUser(all, "uuid-spoof")
	if len(queued) != 1 || queued[0].Type != "inject" {
		t.Fatalf("в очереди должен быть только inject, получено: %+v", queued)
	}
	raw, _ := svc.ListDetections(ctx, 500, DetectionFilter{Status: detectionStatusUnmatched})
	if len(byUser(raw, "uuid-spoof")) != 2 {
		t.Fatalf("подделанные детекты должны сохраняться как unmatched, получено: %+v", byUser(raw, "uuid-spoof"))
	}
	stats, _ := svc.SignatureStats(ctx, time.Unix(0, 0))
	for _, st := range stats {
		if strings.HasPrefix(st.Signature, "spoof-junk") {
			t.Fatalf("статистика сигнатур не должна учитывать unmatched: %+v", st)
		}
	}
}

// Алерт шлётся только за совпавший детект: иначе любой игрок с curl'ом флудит оператора.
func TestUnmatchedDetectionDoesNotAlert(t *testing.T) {
	svc := NewService(newTestDB(t), "secret", false, nil, "")
	n := &fakeNotifier{}
	svc.SetNotifier(n)
	ctx := context.Background()

	if _, err := svc.InitHandshake(ctx, "uuid-noalert", "svocraft", "", []DetectionInput{
		{Type: "signature", Signature: "spoof-junk-c"},
	}); err != nil {
		t.Fatalf("init: %v", err)
	}
	// Алерт уходит в goroutine — даём ей шанс отработать, чтобы тест не был ложно зелёным.
	time.Sleep(50 * time.Millisecond)
	n.mu.Lock()
	got := n.detection
	n.mu.Unlock()
	if got != 0 {
		t.Fatalf("несовпавший детект не должен алертить, алертов: %d", got)
	}
}

// Длинные клиентские строки обрезаются: иначе поддельный клиент раздувает БД и алерты.
func TestDetectionFieldsAreTruncated(t *testing.T) {
	svc := NewService(newTestDB(t), "secret", false, nil, "")
	ctx := context.Background()

	long := strings.Repeat("a", 5000)
	if _, err := svc.InitHandshake(ctx, "uuid-long", "Bob", "", []DetectionInput{{Type: long, Signature: long}}); err != nil {
		t.Fatalf("init: %v", err)
	}
	raw, _ := svc.ListDetections(ctx, 500, DetectionFilter{Status: detectionStatusUnmatched})
	rows := byUser(raw, "uuid-long")
	if len(rows) != 1 {
		t.Fatalf("ожидалась одна запись, получено %d", len(rows))
	}
	if len(rows[0].Type) != maxDetectionField || len(rows[0].Signature) != maxDetectionField {
		t.Fatalf("поля не обрезаны: type=%d signature=%d", len(rows[0].Type), len(rows[0].Signature))
	}
}

func TestValidHwidHash(t *testing.T) {
	valid := strings.Repeat("ab12", 16) // 64 hex
	cases := map[string]bool{
		"":                      true, // клиент не собрал отпечаток
		valid:                   true,
		"1":                     false,
		strings.Repeat("a", 63): false,
		strings.Repeat("a", 65): false,
		strings.Repeat("A", 64): false, // хеши лаунчера в нижнем регистре
		strings.Repeat("z", 64): false,
	}
	for in, want := range cases {
		if got := validHwidHash(in); got != want {
			t.Fatalf("validHwidHash(%q) = %v, ожидалось %v", in, got, want)
		}
	}
	if validHwidComponents(HwidComponents{MachineID: "1"}) {
		t.Fatal("невалидный machineId должен отклоняться")
	}
	if !validHwidComponents(HwidComponents{MachineID: valid, Macs: []string{valid}}) {
		t.Fatal("корректные компоненты должны приниматься")
	}
	macs := make([]string, maxHwidMacs+1)
	for i := range macs {
		macs[i] = valid
	}
	if validHwidComponents(HwidComponents{Macs: macs}) {
		t.Fatal("слишком много MAC-хешей должно отклоняться")
	}
}
