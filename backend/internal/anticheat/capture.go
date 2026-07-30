package anticheat

import (
	"bytes"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"image/jpeg"
	"image/png"
	"time"
)

// Подпись кадра. Захват экрана делает нативный агент внутри игровой JVM (убить его
// отдельно, как раньше лаунчер, нельзя), а несёт кадр на бэкенд Java-агент — у нативки
// нет HTTPS. Значит, по дороге кадр проходит через JVM, где живёт и потенциальный
// чит-мод: он знает launch-token (-Dac.token) и мог бы залить «чистую» картинку вместо
// реальной. Поэтому нативка подписывает ПИКСЕЛИ ключом, которого в JVM нет:
//
//	captureSecret рождается здесь, на init, уходит лаунчеру по TLS и кладётся им в файл
//	рядом с нативкой; нативка читает файл в Agent_OnLoad и стирает его ДО того, как в
//	этой JVM выполнится первый Java-класс.
//
// Клиент шлёт PNG (без потерь) — бэкенд разжимает его, берёт те же RGB-байты и сверяет
// HMAC. В JPEG для хранения перекодируем уже сами, после проверки.
const (
	// captureSecretTTL — сколько храним секрет сессии. Игровая сессия живёт часами;
	// после — запись всё равно бесполезна (nonce мёртв).
	captureSecretTTL = 24 * time.Hour
	// maxCaptureDimension — потолок стороны кадра. Нативка ужимает до 1920 по ширине;
	// проверка защищает декодер от «PNG-бомбы» с гигапиксельным холстом.
	maxCaptureDimension = 8192
)

type captureEntry struct {
	secret []byte
	at     time.Time
}

// rememberCaptureSecret запоминает секрет подписи кадров для сессии (по nonce).
func (s *Service) rememberCaptureSecret(nonce string, secret []byte) {
	if nonce == "" || len(secret) == 0 {
		return
	}
	now := s.now()
	s.captureMu.Lock()
	defer s.captureMu.Unlock()
	for k, e := range s.captureSecrets {
		if now.Sub(e.at) > captureSecretTTL {
			delete(s.captureSecrets, k)
		}
	}
	s.captureSecrets[nonce] = captureEntry{secret: secret, at: now}
}

func (s *Service) captureSecret(nonce string) []byte {
	s.captureMu.Lock()
	defer s.captureMu.Unlock()
	e, ok := s.captureSecrets[nonce]
	if !ok || s.now().Sub(e.at) > captureSecretTTL {
		return nil
	}
	return e.secret
}

// VerifiedCaptureJPEG проверяет подпись присланного PNG-кадра и возвращает его же,
// перекодированный в JPEG для хранения. Ошибка = подпись не сошлась или кадр битый;
// такой скриншот принимать нельзя — он мог быть подменён модом внутри игры.
func (s *Service) VerifiedCaptureJPEG(nonce, id string, width, height int, pngData []byte, sigHex string) ([]byte, error) {
	secret := s.captureSecret(nonce)
	if len(secret) == 0 {
		return nil, fmt.Errorf("нет секрета подписи для сессии")
	}
	if width <= 0 || height <= 0 || width > maxCaptureDimension || height > maxCaptureDimension {
		return nil, fmt.Errorf("некорректный размер кадра")
	}
	img, err := png.Decode(bytes.NewReader(pngData))
	if err != nil {
		return nil, fmt.Errorf("кадр не PNG: %w", err)
	}
	bounds := img.Bounds()
	if bounds.Dx() != width || bounds.Dy() != height {
		return nil, fmt.Errorf("размер кадра не совпадает с заявленным")
	}

	// Те же байты, что нативка подписывала: RGB24 построчно.
	// ponytail: At() на пиксель — ~1.5 млн вызовов на 1080p (~50мс). Скриншот запрашивают
	// поштучно и вручную, так что типизированный fast path под *image.RGBA не нужен.
	mac := hmac.New(sha256.New, secret)
	fmt.Fprintf(mac, "%s\n%d\n%d\n", id, width, height)
	row := make([]byte, 0, width*3)
	for y := bounds.Min.Y; y < bounds.Max.Y; y++ {
		row = row[:0]
		for x := bounds.Min.X; x < bounds.Max.X; x++ {
			r, g, b, _ := img.At(x, y).RGBA()
			row = append(row, byte(r>>8), byte(g>>8), byte(b>>8))
		}
		mac.Write(row)
	}
	want, err := hex.DecodeString(sigHex)
	if err != nil || !hmac.Equal(mac.Sum(nil), want) {
		return nil, fmt.Errorf("подпись кадра не сошлась")
	}

	var out bytes.Buffer
	if err := jpeg.Encode(&out, img, &jpeg.Options{Quality: 80}); err != nil {
		return nil, fmt.Errorf("перекодирование в JPEG: %w", err)
	}
	return out.Bytes(), nil
}
