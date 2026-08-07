// Package gameapi — эндпоинты для игрового сервера Minecraft (мод PJM BaseMod).
// Аутентификация — общий секрет в заголовке X-Game-Secret, как у P5-хендшейка
// античита: у игрового сервера нет пользовательской сессии, а mTLS здесь избыточен.
package gameapi

import (
	"crypto/subtle"
	"net/http"
	"time"

	"launcher-backend/internal/models"

	"github.com/gofiber/fiber/v3"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

type Handler struct {
	db     *gorm.DB
	secret string
}

func NewHandler(db *gorm.DB, secret string) Handler {
	return Handler{db: db, secret: secret}
}

func (h Handler) RegisterRoutes(app *fiber.App) {
	g := app.Group("/api/game")
	g.Use(h.requireSecret)
	g.Post("/players/sync", h.syncPlayers)
	g.Post("/adjustments/poll", h.pollAdjustments)
}

// requireSecret — постоянное по времени сравнение секрета. Пустой секрет на сервере
// закрывает эндпоинты полностью (безопасный дефолт), а не открывает их всем.
func (h Handler) requireSecret(c fiber.Ctx) error {
	if h.secret == "" ||
		subtle.ConstantTimeCompare([]byte(c.Get("X-Game-Secret")), []byte(h.secret)) != 1 {
		return c.Status(http.StatusUnauthorized).JSON(fiber.Map{"message": "unauthorized"})
	}
	return c.Next()
}

type syncPlayer struct {
	UUID   string `json:"uuid"`
	Name   string `json:"name"`
	XP     int64  `json:"xp"`
	RankID string `json:"rankId"`
}

type syncRequest struct {
	Players []syncPlayer `json:"players"`
}

// syncPlayers — батч-upsert снимка прогресса. Мод шлёт только изменившихся игроков.
func (h Handler) syncPlayers(c fiber.Ctx) error {
	var req syncRequest
	if err := c.Bind().Body(&req); err != nil {
		return c.Status(http.StatusBadRequest).JSON(fiber.Map{"message": "bad request"})
	}
	if len(req.Players) == 0 {
		return c.JSON(fiber.Map{"ok": true, "saved": 0})
	}

	now := time.Now()
	// Дедуп по UUID, последняя запись в батче побеждает: Postgres роняет ВЕСЬ
	// многострочный INSERT ... ON CONFLICT DO UPDATE, если один conflict-key
	// (uuid) задет дважды в одном statement ("cannot affect row a second time"),
	// а не только строку-дубль. Дедупим до Create, чтобы одинаковый uuid
	// встречался в батче не больше раза.
	byUUID := make(map[string]models.PlayerProfile, len(req.Players))
	order := make([]string, 0, len(req.Players))
	for _, p := range req.Players {
		if p.UUID == "" {
			continue
		}
		if _, seen := byUUID[p.UUID]; !seen {
			order = append(order, p.UUID)
		}
		byUUID[p.UUID] = models.PlayerProfile{
			UUID: p.UUID, Name: p.Name, XP: p.XP, RankID: p.RankID, UpdatedAt: now,
		}
	}
	if len(order) == 0 {
		return c.JSON(fiber.Map{"ok": true, "saved": 0})
	}
	rows := make([]models.PlayerProfile, 0, len(order))
	for _, uuid := range order {
		rows = append(rows, byUUID[uuid])
	}

	err := h.db.Clauses(clause.OnConflict{
		Columns: []clause.Column{{Name: "uuid"}},
		DoUpdates: clause.Assignments(map[string]interface{}{
			"xp":         gorm.Expr("excluded.xp"),
			"rank_id":    gorm.Expr("excluded.rank_id"),
			"updated_at": gorm.Expr("excluded.updated_at"),
			// Пустой ник — промах кеша профилей у офлайн-игрока, а не «ника нет».
			// Затирать им уже известный нельзя: заметнее всего после массового сброса
			// рангов, который помечает грязными всех исторических игроков разом.
			"name": gorm.Expr("COALESCE(NULLIF(excluded.name, ''), player_profiles.name)"),
		}),
		// Нарезка батча: 500 строк × 5 значений заведомо ниже потолка Postgres в 65535
		// параметров на statement, в который упирался массовый вайп (~13k профилей).
	}).CreateInBatches(&rows, 500).Error
	if err != nil {
		return c.Status(http.StatusInternalServerError).JSON(fiber.Map{"message": "save failed"})
	}
	return c.JSON(fiber.Map{"ok": true, "saved": len(rows)})
}

type pollRequest struct {
	// Ack — id правок, применённых модом с прошлого опроса.
	Ack []uint `json:"ack"`
}

// pollAdjustments — ACK применённых + выдача неприменённых. Один round-trip:
// отдельная ручка подтверждения означала бы окно, в котором правка применена
// в игре, но на бэкенде ещё висит как ожидающая, и прилетела бы второй раз.
func (h Handler) pollAdjustments(c fiber.Ctx) error {
	var req pollRequest
	if err := c.Bind().Body(&req); err != nil {
		return c.Status(http.StatusBadRequest).JSON(fiber.Map{"message": "bad request"})
	}

	if len(req.Ack) > 0 {
		now := time.Now()
		if err := h.db.Model(&models.PlayerXpAdjustment{}).
			Where("id IN ? AND applied_at IS NULL", req.Ack).
			Update("applied_at", now).Error; err != nil {
			return c.Status(http.StatusInternalServerError).JSON(fiber.Map{"message": "ack failed"})
		}
	}

	var pending []models.PlayerXpAdjustment
	if err := h.db.Where("applied_at IS NULL").Order("id ASC").Limit(500).Find(&pending).Error; err != nil {
		return c.Status(http.StatusInternalServerError).JSON(fiber.Map{"message": "query failed"})
	}
	return c.JSON(fiber.Map{"adjustments": pending})
}
