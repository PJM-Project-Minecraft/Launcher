package shop

import (
	"errors"
	"net/http"
	"strconv"
	"strings"

	"launcher-backend/internal/auth"

	"github.com/gofiber/fiber/v3"
	"gorm.io/gorm"
)

type Handler struct{ service Service }

func NewHandler(service Service) Handler { return Handler{service: service} }

func (h Handler) RegisterRoutes(app *fiber.App, authMiddleware fiber.Handler) {
	public := app.Group("/api/shop")
	public.Get("/catalog", h.catalog)
	public.Get("/images/:file", h.image)

	admin := app.Group("/api/admin/shop/items")
	admin.Use(authMiddleware, auth.RequireAdmin)
	admin.Get("/", h.list)
	admin.Post("/", h.create)
	admin.Put("/:id", h.update)
	admin.Delete("/:id", h.delete)
	admin.Post("/:id/image", h.uploadImage)
}

func itemID(raw string) (int64, error) {
	id, err := strconv.ParseInt(raw, 10, 64)
	if err != nil || id <= 0 {
		return 0, validation("Некорректный id товара")
	}
	return id, nil
}

func writeError(c fiber.Ctx, err error) error {
	switch {
	case errors.Is(err, gorm.ErrRecordNotFound):
		return c.Status(http.StatusNotFound).JSON(fiber.Map{"message": "Товар не найден"})
	case errors.Is(err, ErrValidation):
		parts := strings.SplitN(err.Error(), ": ", 2)
		message := err.Error()
		if len(parts) == 2 {
			message = parts[1]
		}
		return c.Status(http.StatusBadRequest).JSON(fiber.Map{"message": message})
	default:
		return c.Status(http.StatusInternalServerError).JSON(fiber.Map{"message": "Ошибка каталога"})
	}
}

func (h Handler) catalog(c fiber.Ctx) error {
	items, err := h.service.List(c.Context(), true)
	if err != nil {
		return writeError(c, err)
	}
	c.Set(fiber.HeaderCacheControl, "public, max-age=60, stale-while-revalidate=300")
	return c.JSON(items)
}

func (h Handler) list(c fiber.Ctx) error {
	items, err := h.service.List(c.Context(), false)
	if err != nil {
		return writeError(c, err)
	}
	return c.JSON(items)
}

func (h Handler) create(c fiber.Ctx) error {
	var input ItemInput
	if err := c.Bind().Body(&input); err != nil {
		return c.Status(http.StatusBadRequest).JSON(fiber.Map{"message": "Некорректный запрос"})
	}
	item, err := h.service.Create(c.Context(), input)
	if err != nil {
		return writeError(c, err)
	}
	return c.Status(http.StatusCreated).JSON(item)
}

func (h Handler) update(c fiber.Ctx) error {
	id, err := itemID(c.Params("id"))
	if err != nil {
		return writeError(c, err)
	}
	var input ItemInput
	if err := c.Bind().Body(&input); err != nil {
		return c.Status(http.StatusBadRequest).JSON(fiber.Map{"message": "Некорректный запрос"})
	}
	item, err := h.service.Update(c.Context(), id, input)
	if err != nil {
		return writeError(c, err)
	}
	return c.JSON(item)
}

func (h Handler) delete(c fiber.Ctx) error {
	id, err := itemID(c.Params("id"))
	if err != nil {
		return writeError(c, err)
	}
	if err := h.service.Delete(c.Context(), id); err != nil {
		return writeError(c, err)
	}
	return c.SendStatus(http.StatusNoContent)
}

func (h Handler) uploadImage(c fiber.Ctx) error {
	id, err := itemID(c.Params("id"))
	if err != nil {
		return writeError(c, err)
	}
	form, err := c.MultipartForm()
	if err != nil || len(form.File["image"]) == 0 {
		return c.Status(http.StatusBadRequest).JSON(fiber.Map{"message": "PNG не приложен"})
	}
	opened, err := form.File["image"][0].Open()
	if err != nil {
		return c.Status(http.StatusBadRequest).JSON(fiber.Map{"message": "Не удалось прочитать PNG"})
	}
	defer opened.Close()
	item, err := h.service.SaveImage(c.Context(), id, opened)
	if err != nil {
		return writeError(c, err)
	}
	return c.JSON(item)
}

func (h Handler) image(c fiber.Ctx) error {
	file := c.Params("file")
	if !strings.HasSuffix(file, ".png") {
		return c.SendStatus(http.StatusNotFound)
	}
	id, err := itemID(strings.TrimSuffix(file, ".png"))
	if err != nil {
		return c.SendStatus(http.StatusNotFound)
	}
	path, err := h.service.ImagePath(c.Context(), id)
	if err != nil {
		return c.SendStatus(http.StatusNotFound)
	}
	c.Type("png")
	c.Set(fiber.HeaderCacheControl, "public, max-age=86400")
	return c.SendFile(path)
}
