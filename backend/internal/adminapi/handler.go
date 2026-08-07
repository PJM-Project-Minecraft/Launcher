// Package adminapi — HTTP-эндпоинты управления пользователями для Next.js dashboard.
// Заменяет прежнюю Go-шаблонную админ-панель Telegram-бота.
package adminapi

import (
	"net/http"
	"strconv"
	"strings"

	"launcher-backend/internal/auth"
	"launcher-backend/internal/models"
	"launcher-backend/internal/repo"

	"github.com/gofiber/fiber/v3"
	"gorm.io/gorm"
)

type Handler struct {
	db *gorm.DB
}

type errorResponse struct {
	Message string `json:"message"`
}

func NewHandler(db *gorm.DB) Handler { return Handler{db: db} }

func (h Handler) RegisterRoutes(app *fiber.App, authMiddleware fiber.Handler) {
	g := app.Group("/api/admin")
	g.Use(authMiddleware, auth.RequireAdmin)
	g.Get("/stats", h.stats)
	g.Get("/users", h.listUsers)
	g.Get("/users/:id", h.userDetail)
	g.Patch("/users/:id/role", h.updateRole)
	g.Post("/users/:id/ban", h.ban)
	g.Post("/users/:id/unban", h.unban)
	g.Post("/users/:id/hwid-ban", h.hwidBan)
	g.Post("/users/:id/hwid-unban", h.hwidUnban)
	g.Delete("/users/:id", h.deleteUser)
	g.Get("/auth-logs", h.authLogs)
	g.Get("/audit-logs", h.auditLogs)
	g.Get("/game/players", h.listGamePlayers)
	g.Post("/game/players/:uuid/adjust", h.adjustGamePlayerXP)
}

func atoiQuery(c fiber.Ctx, key string, def int) int {
	n, err := strconv.Atoi(c.Query(key))
	if err != nil || n < 1 {
		return def
	}
	return n
}

func (h Handler) stats(c fiber.Ctx) error {
	s, err := repo.FetchAdminStats(c.Context(), h.db)
	if err != nil {
		return c.Status(http.StatusInternalServerError).JSON(errorResponse{Message: "Не удалось получить статистику"})
	}
	return c.JSON(s)
}

func (h Handler) listUsers(c fiber.Ctx) error {
	page := atoiQuery(c, "page", 1)
	items, total, err := repo.ListUsersAdmin(c.Context(), h.db, c.Query("q", ""), page)
	if err != nil {
		return c.Status(http.StatusInternalServerError).JSON(errorResponse{Message: "Не удалось получить список"})
	}
	return c.JSON(fiber.Map{"items": items, "total": total, "page": page, "pageSize": repo.AdminPageSize})
}

func (h Handler) userDetail(c fiber.Ctx) error {
	detail, err := repo.GetUserDetail(c.Context(), h.db, c.Params("id"))
	if err != nil {
		return c.Status(http.StatusInternalServerError).JSON(errorResponse{Message: "Ошибка"})
	}
	if detail == nil {
		return c.Status(http.StatusNotFound).JSON(errorResponse{Message: "Пользователь не найден"})
	}
	return c.JSON(detail)
}

func (h Handler) updateRole(c fiber.Ctx) error {
	var req struct {
		Role string `json:"role"`
	}
	if err := c.Bind().Body(&req); err != nil || !repo.ValidRole(req.Role) {
		return c.Status(http.StatusBadRequest).JSON(errorResponse{Message: "Недопустимая роль"})
	}
	id := c.Params("id")
	ok, err := repo.UpdateUserRole(c.Context(), h.db, id, req.Role)
	if err != nil || !ok {
		return c.Status(http.StatusInternalServerError).JSON(errorResponse{Message: "Не удалось обновить роль"})
	}
	h.audit(c, id, "admin_role", req.Role)
	return c.SendStatus(http.StatusNoContent)
}

func (h Handler) ban(c fiber.Ctx) error      { return h.setBan(c, true, false) }
func (h Handler) unban(c fiber.Ctx) error     { return h.setBan(c, false, false) }
func (h Handler) hwidBan(c fiber.Ctx) error   { return h.setBan(c, true, true) }
func (h Handler) hwidUnban(c fiber.Ctx) error { return h.setBan(c, false, true) }

func (h Handler) setBan(c fiber.Ctx, banned, hwid bool) error {
	id := c.Params("id")
	var (
		ok  bool
		err error
	)
	if hwid {
		ok, err = repo.SetHwidBan(c.Context(), h.db, id, banned)
	} else {
		ok, err = repo.SetBan(c.Context(), h.db, id, banned)
	}
	if err != nil || !ok {
		return c.Status(http.StatusInternalServerError).JSON(errorResponse{Message: "Не удалось изменить статус блокировки"})
	}
	action := "admin_ban"
	if !banned {
		action = "admin_unban"
	}
	if hwid {
		action = "hwid_" + action
	}
	h.audit(c, id, action, "")
	return c.SendStatus(http.StatusNoContent)
}

func (h Handler) deleteUser(c fiber.Ctx) error {
	id := c.Params("id")
	if err := repo.DeleteUser(c.Context(), h.db, id); err != nil {
		return c.Status(http.StatusInternalServerError).JSON(errorResponse{Message: "Не удалось удалить"})
	}
	h.audit(c, id, "admin_delete", "")
	return c.SendStatus(http.StatusNoContent)
}

func (h Handler) authLogs(c fiber.Ctx) error {
	items, err := repo.ListAuthLogs(c.Context(), h.db, atoiQuery(c, "page", 1))
	if err != nil {
		return c.Status(http.StatusInternalServerError).JSON(errorResponse{Message: "Ошибка"})
	}
	return c.JSON(fiber.Map{"items": items})
}

func (h Handler) auditLogs(c fiber.Ctx) error {
	items, err := repo.ListAuditLogs(c.Context(), h.db, atoiQuery(c, "page", 1))
	if err != nil {
		return c.Status(http.StatusInternalServerError).JSON(errorResponse{Message: "Ошибка"})
	}
	return c.JSON(fiber.Map{"items": items})
}

// audit записывает действие администратора (id из JWT) в журнал.
func (h Handler) audit(c fiber.Ctx, targetID, action, details string) {
	admin, ok := auth.CurrentUser(c)
	if !ok {
		return
	}
	adminID := admin.ID
	target := targetID
	var detailsPtr *string
	if details != "" {
		detailsPtr = &details
	}
	_ = repo.InsertAudit(c.Context(), h.db, nil, &adminID, &target, action, detailsPtr)
}

const gamePlayersPageSize = 50

// listGamePlayers — зеркало игрового прогресса. Только чтение: XP правится
// исключительно через очередь (adjustGamePlayerXP), которую разбирает мод.
func (h Handler) listGamePlayers(c fiber.Ctx) error {
	page := atoiQuery(c, "page", 1)
	query := h.db.Model(&models.PlayerProfile{})
	if q := strings.TrimSpace(c.Query("q", "")); q != "" {
		query = query.Where("name LIKE ?", "%"+q+"%")
	}

	var total int64
	if err := query.Count(&total).Error; err != nil {
		return c.Status(http.StatusInternalServerError).JSON(errorResponse{Message: "Не удалось получить список"})
	}
	var items []models.PlayerProfile
	if err := query.Order("xp DESC").
		Offset((page - 1) * gamePlayersPageSize).Limit(gamePlayersPageSize).
		Find(&items).Error; err != nil {
		return c.Status(http.StatusInternalServerError).JSON(errorResponse{Message: "Не удалось получить список"})
	}
	return c.JSON(fiber.Map{"items": items, "total": total, "page": page, "pageSize": gamePlayersPageSize})
}

// adjustGamePlayerXP — постановка правки XP в очередь. Прямая запись в PlayerProfile
// запрещена намеренно: авторитетен игровой сервер, он же применяет правку и присылает
// новое состояние — иначе пришлось бы разрешать конфликт двух писателей.
func (h Handler) adjustGamePlayerXP(c fiber.Ctx) error {
	var req struct {
		Delta    int64  `json:"delta"`
		SetValue *int64 `json:"setValue"`
		Reason   string `json:"reason"`
	}
	if err := c.Bind().Body(&req); err != nil {
		return c.Status(http.StatusBadRequest).JSON(errorResponse{Message: "Некорректный запрос"})
	}
	if req.Delta == 0 && req.SetValue == nil {
		return c.Status(http.StatusBadRequest).JSON(errorResponse{Message: "Нужен delta или setValue"})
	}

	createdBy := ""
	if user, ok := auth.CurrentUser(c); ok {
		createdBy = user.Login
	}
	adj := models.PlayerXpAdjustment{
		UUID:      c.Params("uuid"),
		Delta:     req.Delta,
		SetValue:  req.SetValue,
		Reason:    req.Reason,
		CreatedBy: createdBy,
	}
	if err := h.db.Create(&adj).Error; err != nil {
		return c.Status(http.StatusInternalServerError).JSON(errorResponse{Message: "Не удалось создать правку"})
	}
	return c.Status(http.StatusCreated).JSON(adj)
}
