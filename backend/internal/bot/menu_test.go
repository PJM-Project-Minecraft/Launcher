package bot

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"launcher-backend/internal/models"
)

// flatButtons разворачивает inline_keyboard в плоский список кнопок для проверок.
func flatButtons(t *testing.T, markup map[string]any) []map[string]string {
	t.Helper()
	raw, err := json.Marshal(markup)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var parsed struct {
		Keyboard [][]map[string]string `json:"inline_keyboard"`
	}
	if err := json.Unmarshal(raw, &parsed); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	var out []map[string]string
	for _, row := range parsed.Keyboard {
		out = append(out, row...)
	}
	return out
}

func hasCallback(btns []map[string]string, data string) bool {
	for _, b := range btns {
		if b["callback_data"] == data {
			return true
		}
	}
	return false
}

func testUser() *models.User {
	return &models.User{
		Login:       "player1",
		Email:       "player1@mail.test",
		Role:        models.RoleUser,
		TOTPEnabled: false,
		CreatedAt:   time.Date(2026, 1, 15, 12, 0, 0, 0, time.UTC),
	}
}

// TestBuildHomeScreenLinked: привязанный не-админ видит 6 кнопок разделов и не видит админку.
func TestBuildHomeScreenLinked(t *testing.T) {
	v := menuView{User: testUser(), Brand: "PJM", Tagline: "t", DonateURL: "https://shop.test"}
	text, markup := buildHomeScreen(v, "")
	btns := flatButtons(t, markup)
	for _, want := range []string{cbProfile, cbPwd, cbEmail, cb2FA, cbDonate, cbLauncher} {
		if !hasCallback(btns, want) {
			t.Errorf("нет кнопки %s", want)
		}
	}
	if hasCallback(btns, cbAdmin) {
		t.Errorf("админка не должна показываться обычному игроку")
	}
	if hasCallback(btns, cbLogin) || hasCallback(btns, cbRegister) {
		t.Errorf("привязанному не показываются Войти/Регистрация")
	}
	if !strings.Contains(text, "player1") {
		t.Errorf("в шапке нет логина: %q", text)
	}
}

// TestBuildHomeScreenAdmin: админу добавляется кнопка админки.
func TestBuildHomeScreenAdmin(t *testing.T) {
	v := menuView{User: testUser(), Admin: true, Brand: "PJM"}
	_, markup := buildHomeScreen(v, "")
	if !hasCallback(flatButtons(t, markup), cbAdmin) {
		t.Errorf("нет кнопки админки")
	}
}

// TestBuildHomeScreenUnlinked: не привязан — Войти/Регистрация + Донат/Лаунчер, без приватных разделов.
func TestBuildHomeScreenUnlinked(t *testing.T) {
	v := menuView{Brand: "PJM"}
	_, markup := buildHomeScreen(v, "")
	btns := flatButtons(t, markup)
	if !hasCallback(btns, cbLogin) || !hasCallback(btns, cbRegister) {
		t.Errorf("нет Войти/Регистрация")
	}
	for _, bad := range []string{cbProfile, cbPwd, cbEmail, cb2FA} {
		if hasCallback(btns, bad) {
			t.Errorf("приватная кнопка %s у не привязанного", bad)
		}
	}
}

// TestBuildHomeScreenNotice: notice попадает в начало текста.
func TestBuildHomeScreenNotice(t *testing.T) {
	v := menuView{User: testUser(), Brand: "PJM"}
	text, _ := buildHomeScreen(v, "✅ Пароль обновлён.")
	if !strings.HasPrefix(text, "✅ Пароль обновлён.") {
		t.Errorf("notice не в начале: %q", text)
	}
}

// TestBuildDonateScreenURLButton: экран доната несёт URL-кнопку магазина и Назад.
func TestBuildDonateScreenURLButton(t *testing.T) {
	v := menuView{User: testUser(), DonateURL: "https://shop.test"}
	_, markup := buildDonateScreen(v)
	btns := flatButtons(t, markup)
	foundURL := false
	for _, b := range btns {
		if b["url"] == "https://shop.test" {
			foundURL = true
		}
	}
	if !foundURL {
		t.Errorf("нет URL-кнопки магазина")
	}
	if !hasCallback(btns, cbHome) {
		t.Errorf("нет кнопки Назад")
	}
}

// TestBuildLauncherScreen: URL-кнопка только при непустом LauncherURL,
// кнопка «файл в чат» — только при HasLauncherFile без релизов,
// кнопки платформ — когда есть соответствующий релиз.
func TestBuildLauncherScreen(t *testing.T) {
	v := menuView{User: testUser(), LauncherURL: "", HasLauncherFile: false}
	_, markup := buildLauncherScreen(v)
	btns := flatButtons(t, markup)
	for _, b := range btns {
		if b["url"] != "" {
			t.Errorf("URL-кнопка при пустом LauncherURL: %v", b)
		}
	}
	if hasCallback(btns, cbLauncherFile) {
		t.Errorf("кнопка файла без файла")
	}

	v2 := menuView{User: testUser(), LauncherURL: "https://dl.test/l.exe", HasLauncherFile: true}
	_, markup2 := buildLauncherScreen(v2)
	btns2 := flatButtons(t, markup2)
	if !hasCallback(btns2, cbLauncherFile) {
		t.Errorf("нет кнопки файла (фолбэк без релизов)")
	}
	if hasCallback(btns2, cbLauncherLinux) || hasCallback(btns2, cbLauncherWindows) {
		t.Errorf("кнопки платформ не должны показываться без релизов")
	}

	// Есть релизы — показываем кнопки платформ, фолбэк «файл в чат» скрывается.
	v3 := menuView{
		User:            testUser(),
		LauncherURL:     "https://dl.test/l.exe",
		HasLauncherFile: true,
		LauncherLinux:   &launcherReleaseInfo{Version: "0.3.8", FileName: "launcher", AbsPath: "/tmp/l", Size: 1},
		LauncherWindows: &launcherReleaseInfo{Version: "0.3.8", FileName: "launcher.exe", AbsPath: "/tmp/w", Size: 1},
	}
	_, markup3 := buildLauncherScreen(v3)
	btns3 := flatButtons(t, markup3)
	if !hasCallback(btns3, cbLauncherLinux) {
		t.Errorf("нет кнопки Linux при наличии релиза")
	}
	if !hasCallback(btns3, cbLauncherWindows) {
		t.Errorf("нет кнопки Windows при наличии релиза")
	}
	if hasCallback(btns3, cbLauncherFile) {
		t.Errorf("фолбэк «файл в чат» не должен показываться при наличии релизов")
	}

	// Только Linux-релиз — кнопка Windows не появляется.
	v4 := menuView{User: testUser(), LauncherLinux: &launcherReleaseInfo{Version: "0.3.8", FileName: "launcher", AbsPath: "/tmp/l", Size: 1}}
	_, markup4 := buildLauncherScreen(v4)
	btns4 := flatButtons(t, markup4)
	if !hasCallback(btns4, cbLauncherLinux) {
		t.Errorf("нет кнопки Linux")
	}
	if hasCallback(btns4, cbLauncherWindows) {
		t.Errorf("кнопка Windows не должна показываться без Windows-релиза")
	}
}

// TestBuild2FAScreenToggle: выключена — кнопка включения; включена — кнопка выключения.
func TestBuild2FAScreenToggle(t *testing.T) {
	off := menuView{User: testUser()}
	_, markupOff := build2FAScreen(off)
	if !hasCallback(flatButtons(t, markupOff), cb2FAOn) {
		t.Errorf("нет кнопки включения 2FA")
	}
	u := testUser()
	u.TOTPEnabled = true
	on := menuView{User: u}
	_, markupOn := build2FAScreen(on)
	if !hasCallback(flatButtons(t, markupOn), cb2FAOff) {
		t.Errorf("нет кнопки выключения 2FA")
	}
}

// TestHomeReplyKeyboardSingleButton: reply-клавиатура — ровно одна кнопка «🏠 Меню».
func TestHomeReplyKeyboardSingleButton(t *testing.T) {
	raw, _ := json.Marshal(homeReplyKeyboardMarkup())
	var parsed struct {
		Keyboard     [][]map[string]any `json:"keyboard"`
		IsPersistent bool               `json:"is_persistent"`
	}
	if err := json.Unmarshal(raw, &parsed); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(parsed.Keyboard) != 1 || len(parsed.Keyboard[0]) != 1 {
		t.Fatalf("должна быть одна кнопка: %v", parsed.Keyboard)
	}
	if parsed.Keyboard[0][0]["text"] != menuButtonLabel {
		t.Errorf("текст кнопки: %v", parsed.Keyboard[0][0]["text"])
	}
	if !parsed.IsPersistent {
		t.Errorf("клавиатура должна быть persistent")
	}
}

// TestBuildPasswordScreen: раздел «Пароль» несёт смену, заявку админу и Назад.
func TestBuildPasswordScreen(t *testing.T) {
	_, markup := buildPasswordScreen(menuView{User: testUser()})
	btns := flatButtons(t, markup)
	for _, want := range []string{cbPwdChange, cbPwdReset, cbHome} {
		if !hasCallback(btns, want) {
			t.Errorf("нет кнопки %s", want)
		}
	}
}
