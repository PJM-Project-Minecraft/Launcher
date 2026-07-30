package telegram

import (
	"context"
	"net/url"
	"strings"
	"testing"
)

// В URL запроса лежит токен бота, а *url.Error печатает URL целиком: любая сетевая
// ошибка (таймаут, отказ прокси, DNS) утаскивала токен в логи.
func TestUnwrapURLErrHidesToken(t *testing.T) {
	err := unwrapURLErr(&url.Error{
		Op:  "Post",
		URL: "https://api.telegram.org/bot123456789:AAHsecretTOKENvalue/sendMessage",
		Err: context.DeadlineExceeded,
	})
	if strings.Contains(err.Error(), "secretTOKENvalue") {
		t.Fatalf("токен не должен попадать в текст ошибки: %v", err)
	}
	if !strings.Contains(err.Error(), "deadline") {
		t.Fatalf("причина ошибки должна сохраняться: %v", err)
	}
	// Ошибки без URL проходят как есть.
	if got := unwrapURLErr(context.Canceled); got != context.Canceled {
		t.Fatalf("обычная ошибка не должна подменяться: %v", got)
	}
}
