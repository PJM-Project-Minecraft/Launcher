package profiles

import (
	"errors"
	"net/http"
	"strings"

	"launcher-backend/internal/auth"
	"launcher-backend/internal/events"

	"github.com/gofiber/fiber/v3"
	"github.com/gofiber/fiber/v3/middleware/sse"
	"gorm.io/gorm"
)

type Handler struct {
	service Service
	broker  *events.Broker
}

type ErrorResponse struct {
	Message string `json:"message"`
}

func NewHandler(service Service, broker *events.Broker) Handler {
	return Handler{service: service, broker: broker}
}

// profilesEvent — имя SSE-события, по которому клиент перезапрашивает список профилей.
const profilesEvent = "profiles"

func (h Handler) RegisterRoutes(app *fiber.App, authMiddleware fiber.Handler) {
	group := app.Group("/api/profiles")
	group.Use(authMiddleware)
	// Статический /events регистрируем до параметрических маршрутов.
	group.Get("/events", h.events)
	group.Get("/", h.listActive)
	group.Get("/:id/manifest", h.manifest)
	group.Get("/:id/files/*", h.download)

	admin := app.Group("/api/admin/profiles")
	admin.Use(authMiddleware, auth.RequireAdmin)
	admin.Get("/", h.listAll)
	admin.Get("/loader-options", h.loaderOptions)
	admin.Post("/", h.create)
	admin.Get("/:id/manifest", h.manifest)
	admin.Patch("/:id", h.update)
	admin.Delete("/:id", h.delete)
	admin.Post("/:id/prepare-client", h.prepareClient)
	admin.Post("/:id/scan", h.scan)
	admin.Get("/:id/drift", h.drift)
}

// notifyProfilesChanged рассылает подключённым лаунчерам сигнал перезапросить профили.
func (h Handler) notifyProfilesChanged() {
	if h.broker != nil {
		h.broker.Publish(profilesEvent)
	}
}

// events открывает SSE-поток: при каждом изменении профилей клиент получает
// событие "profiles" и перезапрашивает актуальный список.
func (h Handler) events(c fiber.Ctx) error {
	if h.broker == nil {
		return c.SendStatus(http.StatusServiceUnavailable)
	}
	return sse.New(sse.Config{
		Handler: func(_ fiber.Ctx, stream *sse.Stream) error {
			id, ch := h.broker.Subscribe()
			defer h.broker.Unsubscribe(id)
			for {
				select {
				case <-stream.Context().Done():
					return nil
				case msg, ok := <-ch:
					if !ok {
						return nil
					}
					if err := stream.Event(sse.Event{Name: profilesEvent, Data: msg}); err != nil {
						return err
					}
				}
			}
		},
	})(c)
}

func (h Handler) listActive(c fiber.Ctx) error {
	items, err := h.service.ListActive(c.Context())
	if err != nil {
		return c.Status(http.StatusInternalServerError).JSON(ErrorResponse{Message: "Не удалось получить профили"})
	}
	return c.JSON(items)
}

func (h Handler) listAll(c fiber.Ctx) error {
	items, err := h.service.ListAll(c.Context())
	if err != nil {
		return c.Status(http.StatusInternalServerError).JSON(ErrorResponse{Message: "Не удалось получить профили"})
	}
	return c.JSON(items)
}

func (h Handler) loaderOptions(c fiber.Ctx) error {
	return c.JSON(h.service.LoaderOptions(c.Context(), c.Query("gameVersion", "1.21.1")))
}

func (h Handler) create(c fiber.Ctx) error {
	var req ProfileRequest
	if err := c.Bind().Body(&req); err != nil {
		return c.Status(http.StatusBadRequest).JSON(ErrorResponse{Message: "Некорректный JSON"})
	}

	profile, err := h.service.Create(c.Context(), req)
	if err != nil {
		return h.writeError(c, err)
	}
	h.notifyProfilesChanged()
	return c.Status(http.StatusCreated).JSON(profile)
}

func (h Handler) update(c fiber.Ctx) error {
	var req ProfileRequest
	if err := c.Bind().Body(&req); err != nil {
		return c.Status(http.StatusBadRequest).JSON(ErrorResponse{Message: "Некорректный JSON"})
	}

	profile, err := h.service.Update(c.Context(), c.Params("id"), req)
	if err != nil {
		return h.writeError(c, err)
	}
	h.notifyProfilesChanged()
	return c.JSON(profile)
}

func (h Handler) delete(c fiber.Ctx) error {
	if err := h.service.Delete(c.Context(), c.Params("id")); err != nil {
		return h.writeError(c, err)
	}
	h.notifyProfilesChanged()
	return c.SendStatus(http.StatusNoContent)
}

func (h Handler) scan(c fiber.Ctx) error {
	result, err := h.service.Scan(c.Context(), c.Params("id"))
	if err != nil {
		return h.writeError(c, err)
	}
	h.notifyProfilesChanged()
	return c.JSON(result)
}

// drift — read-only сверка storage с манифестом (см. Service.Drift): дашборд
// показывает предупреждение «файлы изменились — нажми Сканировать».
func (h Handler) drift(c fiber.Ctx) error {
	result, err := h.service.Drift(c.Context(), c.Params("id"))
	if err != nil {
		return h.writeError(c, err)
	}
	return c.JSON(result)
}

func (h Handler) prepareClient(c fiber.Ctx) error {
	result, err := h.service.PrepareClient(c.Context(), c.Params("id"))
	if err != nil {
		return h.writeError(c, err)
	}
	h.notifyProfilesChanged()
	return c.JSON(result)
}

func (h Handler) manifest(c fiber.Ctx) error {
	manifest, err := h.service.Manifest(c.Context(), c.Params("id"))
	if err != nil {
		return h.writeError(c, err)
	}
	return c.JSON(manifest)
}

func (h Handler) download(c fiber.Ctx) error {
	download, err := h.service.Download(c.Context(), c.Params("id"), c.Params("*"))
	if err != nil {
		return h.writeError(c, err)
	}
	c.Set(fiber.HeaderContentDisposition, "attachment; filename=\""+safeHeaderFilename(download.File.Name)+"\"")
	c.Set(fiber.HeaderCacheControl, "private, max-age=60")
	return c.SendFile(download.AbsolutePath)
}

func (h Handler) writeError(c fiber.Ctx, err error) error {
	switch {
	case errors.Is(err, gorm.ErrRecordNotFound):
		return c.Status(http.StatusNotFound).JSON(ErrorResponse{Message: "Запись не найдена"})
	case err != nil:
		return c.Status(http.StatusBadRequest).JSON(ErrorResponse{Message: err.Error()})
	default:
		return nil
	}
}

func safeHeaderFilename(name string) string {
	name = strings.ReplaceAll(name, "\r", "")
	name = strings.ReplaceAll(name, "\n", "")
	name = strings.ReplaceAll(name, "\"", "")
	if name == "" {
		return "download"
	}
	return name
}
