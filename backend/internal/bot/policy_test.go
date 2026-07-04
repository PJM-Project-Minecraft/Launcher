package bot

import (
	"encoding/json"
	"strings"
	"testing"

	"launcher-backend/internal/models"
	"launcher-backend/internal/policy"
)

func markupString(t *testing.T, markup map[string]any) string {
	t.Helper()
	b, err := json.Marshal(markup)
	if err != nil {
		t.Fatalf("marshal markup: %v", err)
	}
	return string(b)
}

func TestPolicyGateApplies(t *testing.T) {
	noConsent := menuView{User: &models.User{PolicyAcceptedVersion: 0}}
	consent := menuView{User: &models.User{PolicyAcceptedVersion: policy.Version}}
	unlinked := menuView{}

	if !policyGateApplies(noConsent, cbProfile) {
		t.Error("привязанный без согласия должен блокироваться")
	}
	if policyGateApplies(noConsent, cbPolicyAccept) {
		t.Error("кнопка принятия не должна блокироваться гейтом")
	}
	if policyGateApplies(consent, cbProfile) {
		t.Error("с актуальным согласием гейт не применяется")
	}
	if policyGateApplies(unlinked, cbProfile) {
		t.Error("непривязанных гейт не трогает (их ловит callbackNeedsLink)")
	}
}

func TestBuildPolicyScreen(t *testing.T) {
	text, markup := buildPolicyScreen("https://example.com/privacy")
	if !strings.Contains(text, "скриншоты экрана") {
		t.Error("выжимка должна упоминать скриншоты экрана")
	}
	raw := markupString(t, markup)
	if !strings.Contains(raw, cbPolicyAccept) {
		t.Errorf("нет кнопки принятия: %s", raw)
	}
	if !strings.Contains(raw, "https://example.com/privacy") {
		t.Errorf("нет URL-кнопки полного текста: %s", raw)
	}
}

func TestBuildPolicyScreenNoURL(t *testing.T) {
	// Без PUBLIC_BASE_URL URL-кнопка не добавляется, но принятие работает.
	text, markup := buildPolicyScreen("")
	if !strings.Contains(text, "скриншоты экрана") {
		t.Error("выжимка должна упоминать скриншоты экрана")
	}
	raw := markupString(t, markup)
	if !strings.Contains(raw, cbPolicyAccept) {
		t.Errorf("нет кнопки принятия при пустом URL: %s", raw)
	}
	if strings.Contains(raw, `"url"`) {
		t.Errorf("URL-кнопка не должна быть добавлена при пустом URL: %s", raw)
	}
}

func TestBuildRegPolicyScreen(t *testing.T) {
	text, markup := buildRegPolicyScreen("https://example.com/privacy")
	if !strings.Contains(text, "скриншоты экрана") {
		t.Error("выжимка должна упоминать скриншоты экрана")
	}
	raw := markupString(t, markup)
	if !strings.Contains(raw, cbPolicyRegAccept) {
		t.Errorf("нет кнопки принятия для регистрации: %s", raw)
	}
	if !strings.Contains(raw, cbHome) {
		t.Errorf("нет кнопки «Назад»: %s", raw)
	}
}
