package anticheat

import (
	"context"
	"log/slog"
	"sync"
	"time"

	"launcher-backend/internal/models"
)

// Детект серии проваленных скриншотов.
//
// Кадр забирает Java-агент внутри игровой JVM. Держатель launch-token может просто
// СЪЕДАТЬ запросы: забрать pending и не прислать ни кадра, ни ошибки — запрос уйдёт в
// failed по таймауту, и админ останется без картинки. Подделать кадр читер не может
// (подпись нативки, см. capture.go), а вот заглушить канал — да.
//
// Один провал ничего не значит: игрок свернул игру, моргнула сеть, на Linux нет X11.
// Значение имеет СЕРИЯ подряд. Детект намеренно soft: у честного игрока на Wayland без
// XWayland захват не работает в принципе, и кикать его за это нельзя — сигнал идёт
// админу в очередь разбора и в Telegram, решение за человеком.
const (
	// screenshotFailType — тип детекта (severity берётся из systemSeverity).
	screenshotFailType = "screenshot-failed"
	// screenshotFailStreak — сколько провалов подряд считаем сигналом. Детект уходит на
	// каждом кратном значении (3, 6, 9…), а не на каждом провале — иначе флуд алертами.
	screenshotFailStreak = 3
	// screenshotStreakWindow — пауза, после которой серия считается прерванной: провалы
	// в разные игровые вечера складывать в одну серию бессмысленно.
	screenshotStreakWindow = 30 * time.Minute
)

type shotStreak struct {
	count int
	at    time.Time
}

type shotStreakTracker struct {
	mu      sync.Mutex
	streaks map[string]shotStreak // uuid игрока -> текущая серия провалов
}

// NoteScreenshotOutcome вызывается ScreenshotService на каждый исход запроса скриншота.
// ok=true (кадр получен) обнуляет серию; timeout отделяет «никто не ответил» (подозрительно)
// от «клиент честно сообщил причину» (обычно окружение игрока).
func (s *Service) NoteScreenshotOutcome(ctx context.Context, rec models.Screenshot, reason string, timeout, ok bool) {
	if rec.UserUUID == "" {
		return
	}
	now := s.now()
	s.shots.mu.Lock()
	for k, e := range s.shots.streaks {
		if now.Sub(e.at) > screenshotStreakWindow {
			delete(s.shots.streaks, k)
		}
	}
	if ok {
		delete(s.shots.streaks, rec.UserUUID)
		s.shots.mu.Unlock()
		return
	}
	entry := s.shots.streaks[rec.UserUUID]
	if now.Sub(entry.at) > screenshotStreakWindow {
		entry.count = 0
	}
	entry.count++
	entry.at = now
	s.shots.streaks[rec.UserUUID] = entry
	streak := entry.count
	s.shots.mu.Unlock()

	if streak < screenshotFailStreak || streak%screenshotFailStreak != 0 {
		return
	}
	signature := "screenshot-error"
	if timeout {
		// Никто не забрал запрос или забрал и промолчал — именно так выглядит
		// заглушенный канал.
		signature = "screenshot-timeout"
	}
	slog.Warn("anticheat: серия проваленных скриншотов", "login", rec.Login,
		"uuid", rec.UserUUID, "streak", streak, "reason", reason)
	// sessionID = nonce запроса: детект привязан к той игровой сессии, по которой
	// запрашивали кадр.
	if _, _, err := s.recordDetection(ctx, rec.UserUUID, rec.Login, "", rec.Nonce, DetectionInput{
		Source:    "server",
		Type:      screenshotFailType,
		Signature: signature,
		Details: map[string]any{
			"reason":       reason,
			"streak":       streak,
			"screenshotId": rec.ID,
		},
	}, now); err != nil {
		slog.Warn("anticheat: не удалось записать детект по скриншотам", "error", err)
	}
}
