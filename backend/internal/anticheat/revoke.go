package anticheat

import (
	"log/slog"
	"strings"
	"time"
)

// Реестр отзывов доступа к игре. Замыкает дыру: и «пульс», и самокик исполняет агент
// внутри JVM, то есть процесс, которым читер управляет — прибил агента, и максимум,
// что происходило, это Telegram-алерт. Здесь бэкенд помечает игрока отозванным, а
// исполняет кик ИГРОВОЙ СЕРВЕР (P5-мод поллит /api/anticheat/p5/revoked).
//
// Ключ — логин в нижнем регистре, а не nonce: сессию detect-kick уже мог погасить,
// и мод, знающий только ник, до nonce не доберётся. Запись живёт revokeTTL и снимается
// на успешном Confirm — новый запуск игры (новая сессия) начинает с чистого листа.

// revokeTTL — сколько отзыв остаётся в силе. Должен с запасом перекрывать интервал
// поллинга мода (~25с), чтобы кик пережил пару пропущенных опросов, но не тянуться
// в следующий игровой сеанс: перезапуск игры снимает отзыв через Confirm.
const revokeTTL = 10 * time.Minute

// reasonAgentSilent — отзыв за молчание агента. Единственный, который снимается САМ:
// вернувшийся heartbeat доказывает, что агент жив, и повод отпал. Отзывы по детекту
// («детект: …») так не снимаются — их гасит только новый подтверждённый запуск игры.
const reasonAgentSilent = "агент античита перестал отвечать"

type revocation struct {
	reason string
	at     time.Time
}

// KickOrder — «кого кикнуть» для игрового сервера.
type KickOrder struct {
	Player string `json:"player"`
	Reason string `json:"reason"`
}

// Revoke помечает игрока отозванным: следующий опрос P5-мода вернёт его в списке на
// кик. Повторный вызов не продлевает отзыв — время первого события важнее.
func (s *Service) Revoke(login, reason string) {
	key := strings.ToLower(strings.TrimSpace(login))
	if key == "" {
		return
	}
	s.revokeMu.Lock()
	defer s.revokeMu.Unlock()
	if _, exists := s.revoked[key]; exists {
		return
	}
	s.revoked[key] = revocation{reason: reason, at: s.now()}
	slog.Warn("anticheat: доступ игрока отозван — игровой сервер кикнет по опросу",
		"login", login, "reason", reason)
}

// clearRevocation снимает отзыв (новый подтверждённый запуск игры).
func (s *Service) clearRevocation(login string) {
	key := strings.ToLower(strings.TrimSpace(login))
	if key == "" {
		return
	}
	s.revokeMu.Lock()
	delete(s.revoked, key)
	s.revokeMu.Unlock()
}

// clearSilenceRevocation снимает отзыв, выданный за молчание агента (агент снова на
// связи). Отзыв по детекту не трогает — иначе heartbeat читера отменял бы собственный бан.
func (s *Service) clearSilenceRevocation(login string) {
	key := strings.ToLower(strings.TrimSpace(login))
	if key == "" {
		return
	}
	s.revokeMu.Lock()
	if r, ok := s.revoked[key]; ok && r.reason == reasonAgentSilent {
		delete(s.revoked, key)
		slog.Info("anticheat: отзыв снят — агент снова отвечает", "login", login)
	}
	s.revokeMu.Unlock()
}

// RevokedAmong возвращает приказы на кик для тех из names, чей доступ отозван.
// Заодно выметает протухшие записи — отдельный reaper под это не нужен, список
// опрашивается каждые несколько секунд.
func (s *Service) RevokedAmong(names []string) []KickOrder {
	now := s.now()
	s.revokeMu.Lock()
	defer s.revokeMu.Unlock()
	for key, r := range s.revoked {
		if now.Sub(r.at) > revokeTTL {
			delete(s.revoked, key)
		}
	}
	var out []KickOrder
	for _, name := range names {
		r, ok := s.revoked[strings.ToLower(strings.TrimSpace(name))]
		if !ok {
			continue
		}
		out = append(out, KickOrder{Player: name, Reason: r.reason})
	}
	return out
}
