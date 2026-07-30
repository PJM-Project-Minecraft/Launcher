package anticheat

import (
	"bytes"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"image"
	"image/color"
	"image/png"
	"testing"
)

// framePNG рисует кадр и возвращает (PNG, те же пиксели в RGB24 построчно) — ровно те
// байты, которые подписывает нативный агент.
func framePNG(t *testing.T, w, h int) ([]byte, []byte) {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	rgb := make([]byte, 0, w*h*3)
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			c := color.RGBA{R: byte(x * 7), G: byte(y * 5), B: byte(x + y), A: 255}
			img.Set(x, y, c)
			rgb = append(rgb, c.R, c.G, c.B)
		}
	}
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		t.Fatalf("png: %v", err)
	}
	return buf.Bytes(), rgb
}

func signFrame(secret []byte, id string, w, h int, rgb []byte) string {
	mac := hmac.New(sha256.New, secret)
	fmt.Fprintf(mac, "%s\n%d\n%d\n", id, w, h)
	mac.Write(rgb)
	return hex.EncodeToString(mac.Sum(nil))
}

func TestVerifiedCaptureAcceptsSignedFrame(t *testing.T) {
	svc := NewService(newTestDB(t), "secret", false, nil, "")
	res, err := svc.InitHandshake(t.Context(), "uuid-shot", "Liko", "", nil)
	if err != nil {
		t.Fatalf("init: %v", err)
	}
	if len(res.CaptureSecret) != 64 {
		t.Fatalf("init обязан выдать ключ подписи кадров: %q", res.CaptureSecret)
	}
	secret, err := hex.DecodeString(res.CaptureSecret)
	if err != nil {
		t.Fatalf("secret: %v", err)
	}

	pngData, rgb := framePNG(t, 20, 12)
	sig := signFrame(secret, "shot-1", 20, 12, rgb)

	jpegData, err := svc.VerifiedCaptureJPEG(res.Nonce, "shot-1", 20, 12, pngData, sig)
	if err != nil {
		t.Fatalf("подписанный кадр должен приниматься: %v", err)
	}
	if len(jpegData) < 3 || jpegData[0] != 0xFF || jpegData[1] != 0xD8 {
		t.Fatalf("на выходе ожидается JPEG для хранения, получено %v", jpegData[:3])
	}
}

// PNG-бомба: заявленный размер крошечный, реальный холст — гигантский. Отказ обязан
// произойти по IHDR, до аллокации холста декодером.
func TestVerifiedCaptureRejectsOversizedCanvas(t *testing.T) {
	svc := NewService(newTestDB(t), "secret", false, nil, "")
	res, _ := svc.InitHandshake(t.Context(), "uuid-bomb", "Bomb", "", nil)

	// 8000×8000 однотонных пикселей: PNG сжимается в десятки КБ, RGBA-холст — 256 МБ.
	big := image.NewGray(image.Rect(0, 0, 8000, 8000))
	var buf bytes.Buffer
	if err := png.Encode(&buf, big); err != nil {
		t.Fatalf("png: %v", err)
	}
	if _, err := svc.VerifiedCaptureJPEG(res.Nonce, "shot-bomb", 1, 1, buf.Bytes(), ""); err == nil {
		t.Fatal("кадр с холстом больше заявленного не должен декодироваться")
	}
	// Заявленный размер сверх потолка отвергается ещё раньше — без чтения байтов.
	if _, err := svc.VerifiedCaptureJPEG(res.Nonce, "shot-bomb", 8000, 8000, buf.Bytes(), ""); err == nil {
		t.Fatal("размер сверх maxCaptureDimension не должен приниматься")
	}
}

func TestVerifiedCaptureRejectsTamperedFrame(t *testing.T) {
	svc := NewService(newTestDB(t), "secret", false, nil, "")
	res, _ := svc.InitHandshake(t.Context(), "uuid-shot2", "Bob", "", nil)
	secret, _ := hex.DecodeString(res.CaptureSecret)

	pngData, rgb := framePNG(t, 16, 16)
	sig := signFrame(secret, "shot-2", 16, 16, rgb)

	// Мод внутри JVM подменил кадр «чистой» картинкой, оставив подпись настоящую.
	fake, _ := framePNG(t, 16, 16)
	fake[len(fake)-40]++ // портим содержимое, не ломая структуру PNG-контейнера
	if _, err := svc.VerifiedCaptureJPEG(res.Nonce, "shot-2", 16, 16, fake, sig); err == nil {
		t.Fatal("подменённый кадр не должен приниматься")
	}
	// Тот же кадр, но выданный за чужой запрос — подпись привязана к id.
	if _, err := svc.VerifiedCaptureJPEG(res.Nonce, "other-id", 16, 16, pngData, sig); err == nil {
		t.Fatal("подпись обязана быть привязана к id запроса")
	}
	// Чужая сессия ключа не знает.
	if _, err := svc.VerifiedCaptureJPEG("unknown-nonce", "shot-2", 16, 16, pngData, sig); err == nil {
		t.Fatal("без секрета сессии кадр принимать нельзя")
	}
}
