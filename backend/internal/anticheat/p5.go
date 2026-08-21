package anticheat

import (
	"crypto/hmac"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"log/slog"
	"net/http"
	"strings"

	"github.com/gofiber/fiber/v3"
)

// P5 — серверно-авторитетный in-game handshake (последний замок против «заглушки»
// античита). Обход мьюта работает так: клиент подделывает confirm → yggdrasil-сессия
// помечается Verified → игрок заходит с любым клиентом. P5 переносит проверку на
// ИГРОВОЙ СЕРВЕР (которым читер не управляет): NeoForge-мод на входе игрока челленджит
// клиент и, не получив валидный ответ, кикает.
//
// Протокол: мод шлёт сюда {playerName, challenge, proof}, где proof =
// HMAC-SHA256(challenge, accessToken). accessToken — токен игровой сессии игрока,
// известный и подлинному клиенту (мод берёт его из Minecraft.getUser()), и бэкенду
// (хранит в yggdrasil-Store). Backend сверяет.
//
// ⚠️ Честный потолок: accessToken есть и у читера (он логинился), поэтому кастомный
// клиент, ПЕРЕПИСАВШИЙ протокол мода, теоретически ответит верно. Ценность P5 — не
// криптографическая невозможность, а ПРИНУЖДЕНИЕ ПРИСУТСТВИЯ: массовый чит-клиент,
// не реализующий канал мода, на входе кикается. Вместе с нативным агентом (его грузит
// подлинный мод/лаунчер) и обфускацией это резко поднимает планку.
//
// Аутентификация мода — общий секрет ANTICHEAT_P5_SECRET (server-to-server, не JWT).
type p5Config struct {
	secret  string
	enforce bool // false — репорт-онли (пускаем, логируем); true — кик при невалидном proof
}

type p5Request struct {
	PlayerName string `json:"playerName"`
	Challenge  string `json:"challenge"`
	Proof      string `json:"proof"`
}

// p5Verify — эндпоинт для NeoForge-сервера. allow=false ТОЛЬКО в enforce-режиме при
// невалидном proof; иначе allow=true (репорт-онли расхождения логируются/алертятся).
func (h Handler) p5Verify(c fiber.Ctx) error {
	if h.p5.secret == "" {
		// P5 выключен — не мешаем серверу (мод трактует это как «не кикать»).
		return c.JSON(fiber.Map{"allow": true, "reason": "p5_disabled"})
	}
	if subtle.ConstantTimeCompare([]byte(c.Get("X-AC-P5-Secret")), []byte(h.p5.secret)) != 1 {
		return c.SendStatus(http.StatusUnauthorized)
	}
	var req p5Request
	if err := c.Bind().Body(&req); err != nil || req.PlayerName == "" || req.Challenge == "" {
		return c.Status(http.StatusBadRequest).JSON(fiber.Map{"allow": !h.p5.enforce, "reason": "bad_request"})
	}

	reason, ok := h.p5Check(req.PlayerName, req.Challenge, req.Proof)
	if ok {
		return c.JSON(fiber.Map{"allow": true})
	}
	// Расхождение. Репорт-онли — пускаем, но фиксируем на Error (массовое срабатывание в
	// логах = признак обхода, операторы должны это видеть до включения enforce). Enforce — кик.
	slog.Error("anticheat P5: proof mismatch", "player", req.PlayerName, "reason", reason, "enforce", h.p5.enforce)
	if h.p5.enforce {
		return c.JSON(fiber.Map{"allow": false, "reason": reason})
	}
	return c.JSON(fiber.Map{"allow": true, "reason": reason, "reportOnly": true})
}

// p5Check возвращает (reason, ok). ok=true только при валидном proof активной
// Verified-сессии игрока. Сверяем со ВСЕМИ живыми сессиями ника: после обрыва у игрока
// какое-то время живут две (см. VerifiedSessionsByName), и выбор случайной кикал честных.
func (h Handler) p5Check(name, challenge, proof string) (string, bool) {
	if h.sessions == nil {
		return "no_provider", false
	}
	sessions := h.sessions.VerifiedSessionsByName(name)
	if len(sessions) == 0 {
		return "no_verified_session", false
	}
	got := strings.ToLower(strings.TrimSpace(proof))
	if got == "" {
		// Мод шлёт пустой proof, когда клиент не ответил на challenge за отведённое окно.
		// Отдельная причина, а не bad_proof: «не успел» (слабый канал, загрузка мира) и
		// «ответил неверно» (клиент без мода) требуют разных решений оператора.
		return "no_response", false
	}
	for _, sess := range sessions {
		if subtle.ConstantTimeCompare([]byte(got), []byte(p5Proof(challenge, sess.AccessToken))) == 1 {
			return "", true
		}
	}
	return "bad_proof", false
}

// p5Proof — HMAC-SHA256(challenge) на ключе accessToken, hex. Одинаково считают мод и бэкенд.
func p5Proof(challenge, accessToken string) string {
	mac := hmac.New(sha256.New, []byte(accessToken))
	mac.Write([]byte(challenge))
	return hex.EncodeToString(mac.Sum(nil))
}

// p5RevokedRequest — игровой сервер шлёт ников, кто сейчас онлайн.
type p5RevokedRequest struct {
	Players []string `json:"players"`
}

// maxP5Players — потолок ников в одном опросе (защита от мусорного тела; на сервере
// физически не бывает столько онлайна).
const maxP5Players = 200

// p5Revoked — периодический опрос игрового сервера: «кого из этих кикнуть?». Замыкает
// разрыв, из-за которого молчание heartbeat давало лишь алерт: решение принимает
// бэкенд, исполняет игровой сервер, а не агент внутри JVM игрока.
//
// Репорт-онли (ANTICHEAT_P5_ENFORCE=false) отдаёт ПУСТОЙ список — но логирует, кого бы
// кикнул. Это тот же поэтапный rollout, что у p5Verify: сначала смотрим в логи, потом
// включаем enforce.
func (h Handler) p5Revoked(c fiber.Ctx) error {
	if h.p5.secret == "" {
		return c.JSON(fiber.Map{"kick": []KickOrder{}, "reason": "p5_disabled"})
	}
	if subtle.ConstantTimeCompare([]byte(c.Get("X-AC-P5-Secret")), []byte(h.p5.secret)) != 1 {
		return c.SendStatus(http.StatusUnauthorized)
	}
	var req p5RevokedRequest
	if err := c.Bind().Body(&req); err != nil {
		return c.Status(http.StatusBadRequest).JSON(fiber.Map{"kick": []KickOrder{}})
	}
	if len(req.Players) > maxP5Players {
		req.Players = req.Players[:maxP5Players]
	}
	orders := h.service.RevokedAmong(req.Players)
	if len(orders) == 0 {
		return c.JSON(fiber.Map{"kick": []KickOrder{}})
	}
	if !h.p5.enforce {
		for _, o := range orders {
			slog.Error("anticheat P5: отзыв доступа (репорт-онли, игрок НЕ кикнут)",
				"player", o.Player, "reason", o.Reason)
		}
		return c.JSON(fiber.Map{"kick": []KickOrder{}, "reportOnly": true})
	}
	for _, o := range orders {
		slog.Warn("anticheat P5: кик по отзыву доступа", "player", o.Player, "reason", o.Reason)
	}
	return c.JSON(fiber.Map{"kick": orders})
}
