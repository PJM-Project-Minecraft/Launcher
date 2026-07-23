package launcherrelease

import (
	"errors"
	"mime/multipart"
	"net/http"
	"strings"

	"launcher-backend/internal/auth"
	"launcher-backend/internal/events"

	"github.com/gofiber/fiber/v3"
	"gorm.io/gorm"
)

// releaseEvent — payload SSE-события: лаунчер по нему запускает проверку
// обновления (см. stream_profile_events в launcher-slint).
const releaseEvent = "launcher-release"

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

func (h Handler) RegisterRoutes(app *fiber.App, authMiddleware fiber.Handler) {
	// Публичные: проверка и скачивание обновления работают до логина.
	group := app.Group("/api/launcher")
	group.Get("/update", h.checkUpdate)
	group.Get("/download/:version/:platform", h.download)

	// Публичная витрина скачивания для игроков (ссылка «Скачать с сайта» в боте).
	app.Get("/download", h.downloadPage)
	app.Get("/download/pjm.png", h.logo)

	admin := app.Group("/api/admin/releases")
	admin.Use(authMiddleware, auth.RequireAdmin)
	admin.Get("/", h.list)
	admin.Post("/", h.create)
	admin.Patch("/:id", h.patch)
	admin.Delete("/:id", h.delete)
}

func (h Handler) notifyReleaseChanged() {
	if h.broker != nil {
		h.broker.Publish(releaseEvent)
	}
}

func (h Handler) checkUpdate(c fiber.Ctx) error {
	info, err := h.service.CheckUpdate(c.Context(), c.Query("platform"), c.Query("version", "0.0.0"))
	if err != nil {
		return c.Status(http.StatusBadRequest).JSON(ErrorResponse{Message: err.Error()})
	}
	return c.JSON(info)
}

func (h Handler) download(c fiber.Ctx) error {
	abs, file, err := h.service.Download(c.Context(), c.Params("version"), c.Params("platform"))
	if err != nil {
		return h.writeError(c, err)
	}
	c.Set(fiber.HeaderContentDisposition, "attachment; filename=\""+safeHeaderFilename(file.FileName)+"\"")
	return c.SendFile(abs)
}

func (h Handler) list(c fiber.Ctx) error {
	releases, err := h.service.List(c.Context())
	if err != nil {
		return c.Status(http.StatusInternalServerError).JSON(ErrorResponse{Message: "Не удалось получить релизы"})
	}
	return c.JSON(releases)
}

func (h Handler) create(c fiber.Ctx) error {
	form, err := c.MultipartForm()
	if err != nil {
		return c.Status(http.StatusBadRequest).JSON(ErrorResponse{Message: "Некорректная multipart-форма"})
	}

	req := CreateRequest{
		Version:   formValue(form, "version"),
		Changelog: formValue(form, "changelog"),
		Mandatory: formValue(form, "mandatory") == "true",
	}

	files := make([]UploadedFile, 0, len(AllowedPlatforms))
	for _, platform := range AllowedPlatforms {
		headers := form.File[platform]
		if len(headers) == 0 {
			continue
		}
		opened, err := headers[0].Open()
		if err != nil {
			return c.Status(http.StatusBadRequest).JSON(ErrorResponse{Message: "Не удалось прочитать файл " + platform})
		}
		defer opened.Close()
		files = append(files, UploadedFile{
			Platform:  platform,
			FileName:  headers[0].Filename,
			Reader:    opened,
			Signature: formValue(form, "signature-"+platform),
		})
	}

	release, err := h.service.Create(c.Context(), req, files)
	if err != nil {
		return c.Status(http.StatusBadRequest).JSON(ErrorResponse{Message: err.Error()})
	}
	h.notifyReleaseChanged()
	return c.Status(http.StatusCreated).JSON(release)
}

func (h Handler) patch(c fiber.Ctx) error {
	var req PatchRequest
	if err := c.Bind().Body(&req); err != nil {
		return c.Status(http.StatusBadRequest).JSON(ErrorResponse{Message: "Некорректный JSON"})
	}
	release, err := h.service.Update(c.Context(), c.Params("id"), req)
	if err != nil {
		return h.writeError(c, err)
	}
	h.notifyReleaseChanged()
	return c.JSON(release)
}

func (h Handler) delete(c fiber.Ctx) error {
	if err := h.service.Delete(c.Context(), c.Params("id")); err != nil {
		return h.writeError(c, err)
	}
	h.notifyReleaseChanged()
	return c.SendStatus(http.StatusNoContent)
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

func formValue(form *multipart.Form, key string) string {
	values := form.Value[key]
	if len(values) == 0 {
		return ""
	}
	return values[0]
}

func safeHeaderFilename(name string) string {
	name = strings.ReplaceAll(name, "\r", "")
	name = strings.ReplaceAll(name, "\n", "")
	name = strings.ReplaceAll(name, "\"", "")
	if name == "" {
		return "launcher"
	}
	return name
}
