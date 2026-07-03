package bot

import "testing"

// TestNormalizeCallbackData: telebot добавляет "\f" к data — срезаем.
func TestNormalizeCallbackData(t *testing.T) {
	cases := map[string]string{
		"m:home":       "m:home",
		"\fm:profile":  "m:profile",
		" m:donate ":   "m:donate",
		"\f m:2fa:on ": "m:2fa:on",
	}
	for in, want := range cases {
		if got := normalizeCallbackData(in); got != want {
			t.Errorf("normalizeCallbackData(%q) = %q, want %q", in, got, want)
		}
	}
}

// TestCallbackNeedsLink: приватные экраны требуют привязки, публичные — нет.
func TestCallbackNeedsLink(t *testing.T) {
	private := []string{cbProfile, cbPwd, cbEmail, cb2FA, cb2FAOn, cb2FAOff, cbAdmin}
	public := []string{cbHome, cbDonate, cbLauncher, cbLauncherFile, cbLogin, cbRegister}
	for _, d := range private {
		if !callbackNeedsLink(d) {
			t.Errorf("%s должен требовать привязку", d)
		}
	}
	for _, d := range public {
		if callbackNeedsLink(d) {
			t.Errorf("%s не должен требовать привязку", d)
		}
	}
}
