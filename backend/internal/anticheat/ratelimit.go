package anticheat

import (
	"sync"
	"time"
)

// rateLimiter — простой in-memory лимитер «не более limit событий за window» по ключу.
// Защищает античит-эндпоинты (init/detect/heartbeat) от спама: ключ — uuid игрока, так
// что лимит изолирован по игроку и не штрафует остальных. Память ограничена числом
// активных игроков; протухшие отметки времени отбрасываются на каждом allow.
type rateLimiter struct {
	mu     sync.Mutex
	hits   map[string][]time.Time
	limit  int
	window time.Duration
	now    func() time.Time
}

func newRateLimiter(limit int, window time.Duration) *rateLimiter {
	return &rateLimiter{
		hits:   make(map[string][]time.Time),
		limit:  limit,
		window: window,
		now:    time.Now,
	}
}

// allow регистрирует событие по ключу и возвращает false, если лимит за окно превышен.
// Пустой ключ всегда разрешён (нет идентичности — нечего лимитировать).
func (r *rateLimiter) allow(key string) bool {
	if key == "" {
		return true
	}
	now := r.now()
	cutoff := now.Add(-r.window)
	r.mu.Lock()
	defer r.mu.Unlock()
	// Фильтрация in-place: оставляем только отметки внутри окна.
	kept := r.hits[key][:0]
	for _, t := range r.hits[key] {
		if t.After(cutoff) {
			kept = append(kept, t)
		}
	}
	if len(kept) >= r.limit {
		r.hits[key] = kept
		return false
	}
	r.hits[key] = append(kept, now)
	return true
}
