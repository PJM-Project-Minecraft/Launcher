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
// Verified-сессии игрока.
func (h Handler) p5Check(name, challenge, proof string) (string, bool) {
	if h.sessions == nil {
		return "no_provider", false
	}
	sess, found := h.sessions.VerifiedSessionByName(name)
	if !found {
		return "no_verified_session", false
	}
	expected := p5Proof(challenge, sess.AccessToken)
	got := strings.ToLower(strings.TrimSpace(proof))
	if subtle.ConstantTimeCompare([]byte(got), []byte(expected)) != 1 {
		return "bad_proof", false
	}
	return "", true
}

// p5Proof — HMAC-SHA256(challenge) на ключе accessToken, hex. Одинаково считают мод и бэкенд.
func p5Proof(challenge, accessToken string) string {
	mac := hmac.New(sha256.New, []byte(accessToken))
	mac.Write([]byte(challenge))
	return hex.EncodeToString(mac.Sum(nil))
}
