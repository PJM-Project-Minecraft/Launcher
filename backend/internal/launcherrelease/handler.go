package launcherrelease

import (
	"context"
	"errors"
	"mime/multipart"
	"net/http"
	"strings"
	"time"

	"launcher-backend/internal/auth"
	"launcher-backend/internal/events"
	"launcher-backend/internal/models"

	"github.com/gofiber/fiber/v3"
	"gorm.io/gorm"
)

// releaseEvent — payload SSE-события: лаунчер по нему запускает проверку
// обновления (см. stream_profile_events в src-tauri).
const releaseEvent = "launcher-release"

// MaxReleaseUploadBodySize is intentionally large only for authenticated
// launcher binary multipart uploads. fasthttp spools file parts above its
// in-memory threshold to temporary files while enforcing this total ceiling.
const MaxReleaseUploadBodySize = 512 * 1024 * 1024

type Handler struct {
	service Service
	broker  *events.Broker
	v2      V2Publisher
}

type V2Publisher interface {
	QueueLauncherRelease(context.Context, models.LauncherRelease) (models.DeliveryJob, error)
}

type ErrorResponse struct {
	Message string `json:"message"`
}

func NewHandler(service Service, broker *events.Broker, publishers ...V2Publisher) Handler {
	handler := Handler{service: service, broker: broker}
	if len(publishers) > 0 {
		handler.v2 = publishers[0]
	}
	return handler
}

func (h Handler) RegisterRoutes(app *fiber.App, authMiddleware fiber.Handler) {
	h.RegisterRoutesWithV1Bridge(app, authMiddleware, true)
}

func (h Handler) RegisterRoutesWithV1Bridge(app *fiber.App, authMiddleware fiber.Handler, v1Bridge bool) {
	until := time.Time{}
	if v1Bridge {
		until = time.Date(9999, 1, 1, 0, 0, 0, 0, time.UTC)
	}
	h.RegisterRoutesWithV1BridgeUntil(app, authMiddleware, until)
}

func (h Handler) RegisterRoutesWithV1BridgeUntil(app *fiber.App, authMiddleware fiber.Handler, until time.Time) {
	// Публичные: проверка и скачивание обновления работают до логина.
	if !until.IsZero() {
		group := app.Group("/api/launcher", launcherBridgeDeadline(until))
		group.Get("/update", h.checkUpdate)
		group.Get("/download/:version/:platform", h.download)
	}

	// Публичная витрина скачивания для игроков (ссылка «Скачать с сайта» в боте).
	app.Get("/download", h.downloadPage)
	app.Get("/download/pjm.png", h.logo)

	if !until.IsZero() {
		admin := app.Group("/api/admin/releases", launcherBridgeDeadline(until))
		admin.Use(authMiddleware, auth.RequireAdmin)
		admin.Get("/", h.list)
		admin.Post("/", h.create)
		admin.Patch("/:id", h.patch)
		admin.Delete("/:id", h.delete)
	}

	adminV2 := app.Group("/api/v2/admin/launcher-releases")
	adminV2.Use(authMiddleware, auth.RequireAdmin)
	adminV2.Get("/", h.list)
	adminV2.Post("/", h.create)
	adminV2.Post("/:id/retry", h.retry)
	adminV2.Patch("/:id", h.patch)
	adminV2.Delete("/:id", h.delete)
}

func launcherBridgeDeadline(until time.Time) fiber.Handler {
	return func(c fiber.Ctx) error {
		if !time.Now().UTC().Before(until) {
			return c.SendStatus(http.StatusNotFound)
		}
		return c.Next()
	}
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
	form, err := c.RequestCtx().Request.MultipartFormWithLimit(MaxReleaseUploadBodySize)
	if err != nil {
		return c.Status(http.StatusBadRequest).JSON(ErrorResponse{Message: "Некорректная multipart-форма"})
	}
	defer c.RequestCtx().Request.RemoveMultipartFormFiles()

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

	var release models.LauncherRelease
	if h.v2 != nil {
		release, err = h.service.CreateStaged(c.Context(), req, files)
	} else {
		release, err = h.service.Create(c.Context(), req, files)
	}
	if err != nil {
		return c.Status(http.StatusBadRequest).JSON(ErrorResponse{Message: err.Error()})
	}
	if h.v2 != nil {
		job, queueErr := h.v2.QueueLauncherRelease(c.Context(), release)
		if queueErr != nil {
			_ = h.service.PurgeStaged(c.Context(), release.ID)
			return c.Status(http.StatusBadRequest).JSON(ErrorResponse{Message: "Delivery v2 не принял job: " + queueErr.Error()})
		}
		return c.Status(http.StatusAccepted).JSON(job)
	}
	h.notifyReleaseChanged()
	return c.Status(http.StatusCreated).JSON(release)
}

func (h Handler) retry(c fiber.Ctx) error {
	if h.v2 == nil {
		return c.Status(http.StatusServiceUnavailable).JSON(ErrorResponse{Message: "Delivery v2 не настроен"})
	}
	release, err := h.service.Get(c.Context(), c.Params("id"))
	if err != nil {
		return h.writeError(c, err)
	}
	job, err := h.v2.QueueLauncherRelease(c.Context(), release)
	if err != nil {
		return h.writeError(c, err)
	}
	return c.Status(http.StatusAccepted).JSON(job)
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
