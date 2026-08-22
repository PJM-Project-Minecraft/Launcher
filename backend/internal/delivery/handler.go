package delivery

import (
	"errors"
	"net/http"
	"path/filepath"
	"strconv"
	"strings"

	"launcher-backend/internal/auth"

	"github.com/gofiber/fiber/v3"
	"gorm.io/gorm"
)

type Handler struct{ service *Service }

func NewHandler(service *Service) Handler { return Handler{service: service} }

func (h Handler) RegisterRoutes(app *fiber.App, authMiddleware fiber.Handler) {
	profiles := app.Group("/api/v2/profiles")
	profiles.Use(authMiddleware)
	profiles.Get("/", h.profiles)
	profiles.Get("/:id/releases/:release/manifest", h.profileManifest)
	profiles.Get("/:id/chunks/:hash", h.profileChunk)

	launcher := app.Group("/api/v2/launcher/releases")
	launcher.Get("/current", h.launcherCurrent)
	launcher.Get("/:release/chunks/:hash", h.launcherChunk)
	launcher.Get("/:release/artifact", h.launcherArtifact)

	admin := app.Group("/api/v2/admin/delivery")
	admin.Use(authMiddleware, auth.RequireAdmin)
	admin.Get("/jobs", h.jobs)
	admin.Post("/profiles/:id/drafts", h.createDraft)
	admin.Post("/profiles/:id/drafts/from-active", h.createDraftFromActive)
}

func (h Handler) profiles(c fiber.Ctx) error {
	items, err := h.service.Profiles(c.Context())
	if err != nil {
		return h.writeError(c, err)
	}
	return c.JSON(items)
}

func (h Handler) profileManifest(c fiber.Ctx) error {
	data, release, err := h.service.Manifest(c.Context(), c.Params("id"), c.Params("release"))
	if err != nil {
		return h.writeError(c, err)
	}
	c.Set(fiber.HeaderContentType, fiber.MIMEApplicationJSON)
	c.Set(fiber.HeaderCacheControl, "private, max-age=31536000, immutable")
	c.Set(fiber.HeaderETag, `"`+release.ManifestSHA256+`"`)
	c.Set("X-Manifest-SHA256", release.ManifestSHA256)
	if release.ManifestSignature != "" {
		c.Set("X-Manifest-Signature", release.ManifestSignature)
	}
	return c.Send(data)
}

func (h Handler) profileChunk(c fiber.Ctx) error {
	path, size, err := h.service.Blob(c.Context(), c.Params("id"), c.Params("hash"))
	if err != nil {
		return h.writeError(c, err)
	}
	return sendImmutable(c, path, c.Params("hash"), size, false)
}

func (h Handler) launcherCurrent(c fiber.Ctx) error {
	snapshot, err := h.service.LauncherCurrent(c.Context(), c.Query("platform"), c.Query("from", "0.0.0"))
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return c.SendStatus(http.StatusNoContent)
	}
	if err != nil {
		return h.writeError(c, err)
	}
	c.Set(fiber.HeaderContentType, fiber.MIMEApplicationJSON)
	c.Set(fiber.HeaderCacheControl, "no-cache")
	c.Set("X-Manifest-SHA256", snapshot.SHA256)
	c.Set("X-Manifest-Signature", snapshot.Signature)
	c.Set("X-Update-Mandatory", strconv.FormatBool(snapshot.Mandatory))
	return c.Send(snapshot.Descriptor)
}

func (h Handler) launcherChunk(c fiber.Ctx) error {
	path, size, err := h.service.LauncherBlob(c.Context(), c.Params("release"), c.Params("hash"))
	if err != nil {
		return h.writeError(c, err)
	}
	return sendImmutable(c, path, c.Params("hash"), size, true)
}

func (h Handler) launcherArtifact(c fiber.Ctx) error {
	path, file, err := h.service.LauncherArtifact(c.Context(), c.Params("release"), c.Query("platform"))
	if err != nil {
		return h.writeError(c, err)
	}
	c.Set(fiber.HeaderContentDisposition, `attachment; filename="`+safeFilename(file.FileName)+`"`)
	c.Set(fiber.HeaderCacheControl, "public, max-age=31536000, immutable")
	c.Set(fiber.HeaderETag, `"`+file.HashSHA256+`"`)
	return c.SendFile(path)
}

func (h Handler) jobs(c fiber.Ctx) error {
	jobs, err := h.service.Jobs(c.Context())
	if err != nil {
		return h.writeError(c, err)
	}
	return c.JSON(jobs)
}

func (h Handler) createDraft(c fiber.Ctx) error {
	generation, path, err := h.service.CreateDraft(c.Context(), c.Params("id"))
	if err != nil {
		return h.writeError(c, err)
	}
	return c.Status(http.StatusCreated).JSON(fiber.Map{"generation": generation, "sftpPath": path, "completeBy": "atomic-rename-to-" + generation + ".ready"})
}

func (h Handler) createDraftFromActive(c fiber.Ctx) error {
	draft, err := h.service.CreateDraftFromActive(c.Context(), c.Params("id"))
	if err != nil {
		return h.writeError(c, err)
	}
	return c.Status(http.StatusCreated).JSON(fiber.Map{
		"generation":      draft.Generation,
		"sftpPath":        draft.Path,
		"completeBy":      "atomic-rename-to-" + draft.Generation + ".ready",
		"sourceReleaseId": draft.SourceReleaseID,
		"seededFileCount": draft.SeededFiles,
		"seededTotalSize": draft.SeededSize,
	})
}

func sendImmutable(c fiber.Ctx, path, hash string, size int64, public bool) error {
	visibility := "private"
	if public {
		visibility = "public"
	}
	c.Set(fiber.HeaderCacheControl, visibility+", max-age=31536000, immutable")
	c.Set(fiber.HeaderETag, `"`+hash+`"`)
	c.Set(fiber.HeaderContentLength, strconv.FormatInt(size, 10))
	return c.SendFile(path)
}

func safeFilename(value string) string {
	value = filepath.Base(strings.TrimSpace(value))
	value = strings.NewReplacer("\r", "", "\n", "", `"`, "").Replace(value)
	if value == "" || value == "." {
		return "launcher"
	}
	return value
}

func (h Handler) writeError(c fiber.Ctx, err error) error {
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return c.Status(http.StatusNotFound).JSON(fiber.Map{"message": "Запись не найдена"})
	}
	return c.Status(http.StatusBadRequest).JSON(fiber.Map{"message": err.Error()})
}
