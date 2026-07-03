package telegram

import (
	"encoding/json"
	"testing"
)

// TestInlineMarkup проверяет форму inline_keyboard: callback-кнопка несёт
// callback_data, URL-кнопка — url (и не несёт callback_data).
func TestInlineMarkup(t *testing.T) {
	m := InlineMarkup(
		[]InlineBtn{{Text: "Профиль", Data: "m:profile"}, {Text: "Магазин", URL: "https://shop.example"}},
		[]InlineBtn{{Text: "← Назад", Data: "m:home"}},
	)
	raw, err := json.Marshal(m)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var parsed struct {
		Keyboard [][]map[string]string `json:"inline_keyboard"`
	}
	if err := json.Unmarshal(raw, &parsed); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(parsed.Keyboard) != 2 || len(parsed.Keyboard[0]) != 2 || len(parsed.Keyboard[1]) != 1 {
		t.Fatalf("форма клавиатуры: %v", parsed.Keyboard)
	}
	first := parsed.Keyboard[0][0]
	if first["text"] != "Профиль" || first["callback_data"] != "m:profile" {
		t.Errorf("callback-кнопка: %v", first)
	}
	second := parsed.Keyboard[0][1]
	if second["url"] != "https://shop.example" {
		t.Errorf("url-кнопка: %v", second)
	}
	if _, has := second["callback_data"]; has {
		t.Errorf("url-кнопка не должна нести callback_data: %v", second)
	}
}

// TestLinkPreviewBanner: с URL — превью сверху крупно; без URL — превью выключено.
func TestLinkPreviewBanner(t *testing.T) {
	withURL := LinkPreviewBanner("https://x.example/banner.png")
	if withURL["url"] != "https://x.example/banner.png" ||
		withURL["prefer_large_media"] != true || withURL["show_above_text"] != true {
		t.Errorf("banner options: %v", withURL)
	}
	empty := LinkPreviewBanner("")
	if empty["is_disabled"] != true {
		t.Errorf("пустой URL должен отключать превью: %v", empty)
	}
}
