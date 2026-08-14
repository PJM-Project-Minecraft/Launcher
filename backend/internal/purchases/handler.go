// Package purchases хранит оплаченные заказы сайта и отдаёт их админке.
package purchases

import (
	"crypto/subtle"
	"errors"
	"net/http"
	"strconv"
	"time"

	"launcher-backend/internal/auth"
	"launcher-backend/internal/models"

	"github.com/gofiber/fiber/v3"
	"gorm.io/gorm"
)

type Handler struct {
	service Service
	secret  string
}

func NewHandler(db *gorm.DB, siteSecret string) Handler {
	return Handler{service: NewService(db), secret: siteSecret}
}

func (h Handler) RegisterRoutes(app *fiber.App, authMiddleware fiber.Handler) {
	app.Post("/api/site/orders", h.requireSiteSecret, h.create)
	admin := app.Group("/api/admin/orders")
	admin.Use(authMiddleware, auth.RequireAdmin)
	admin.Get("/stats", h.stats)
	admin.Get("/", h.list)
	admin.Post("/:id/issue", h.issue)
}

func (h Handler) requireSiteSecret(c fiber.Ctx) error {
	provided := c.Get("X-Site-Secret")
	if h.secret == "" || len(provided) != len(h.secret) ||
		subtle.ConstantTimeCompare([]byte(provided), []byte(h.secret)) != 1 {
		return c.Status(http.StatusUnauthorized).JSON(fiber.Map{"message": "unauthorized"})
	}
	return c.Next()
}

func clientError(c fiber.Ctx, err error, fallback string) error {
	var validation ValidationError
	if errors.As(err, &validation) {
		return c.Status(http.StatusBadRequest).JSON(fiber.Map{"message": validation.Message})
	}
	return c.Status(http.StatusInternalServerError).JSON(fiber.Map{"message": fallback})
}

func (h Handler) create(c fiber.Ctx) error {
	var req struct {
		OrderID    string             `json:"orderId"`
		Nick       string             `json:"nick"`
		Items      []models.OrderItem `json:"items"`
		Total      int64              `json:"total"`
		YooKassaID string             `json:"yookassaId"`
		PaidAt     string             `json:"paidAt"`
	}
	if err := c.Bind().Body(&req); err != nil {
		return c.Status(http.StatusBadRequest).JSON(fiber.Map{"message": "Некорректный запрос"})
	}
	paidAt, err := time.Parse(time.RFC3339, req.PaidAt)
	if err != nil {
		return c.Status(http.StatusBadRequest).JSON(fiber.Map{"message": "Некорректное время оплаты"})
	}
	order, created, err := h.service.Create(c.Context(), CreateInput{
		OrderID: req.OrderID, Nick: req.Nick, Items: req.Items, Total: req.Total,
		YooKassaID: req.YooKassaID, PaidAt: paidAt,
	})
	if errors.Is(err, ErrPaymentConflict) {
		return c.Status(http.StatusConflict).JSON(fiber.Map{"message": "Платёж уже привязан к другому заказу"})
	}
	if err != nil {
		return clientError(c, err, "Не удалось сохранить заказ")
	}
	status := http.StatusOK
	if created {
		status = http.StatusCreated
	}
	return c.Status(status).JSON(fiber.Map{"order": order, "created": created})
}

func page(raw string) int {
	value, err := strconv.Atoi(raw)
	if err != nil || value < 1 {
		return 1
	}
	return value
}

func (h Handler) list(c fiber.Ctx) error {
	result, err := h.service.List(c.Context(), ListOptions{
		Status: c.Query("status"), Query: c.Query("q"), From: c.Query("from"),
		To: c.Query("to"), Page: page(c.Query("page")),
	})
	if err != nil {
		return clientError(c, err, "Не удалось получить заказы")
	}
	return c.JSON(result)
}

func (h Handler) issue(c fiber.Ctx) error {
	admin, _ := auth.CurrentUser(c)
	order, err := h.service.Issue(c.Context(), c.Params("id"), admin.ID)
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return c.Status(http.StatusNotFound).JSON(fiber.Map{"message": "Заказ не найден"})
	}
	if err != nil {
		return clientError(c, err, "Не удалось отметить выдачу")
	}
	return c.JSON(order)
}

func (h Handler) stats(c fiber.Ctx) error {
	stats, err := h.service.Stats(c.Context(), c.Query("from"), c.Query("to"))
	if err != nil {
		return clientError(c, err, "Не удалось получить статистику")
	}
	return c.JSON(stats)
}
