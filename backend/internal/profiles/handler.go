package profiles

import (
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"

	"launcher-backend/internal/auth"
	"launcher-backend/internal/events"

	"github.com/gofiber/contrib/v3/websocket"
	"github.com/gofiber/fiber/v3"
	"github.com/gofiber/fiber/v3/middleware/sse"
	"gorm.io/gorm"
)

type Handler struct {
	service Service
	broker  *events.Broker
	builds  *BuildManager
	tickets *SocketTickets
}

type ErrorResponse struct {
	Message string `json:"message"`
}

func NewHandler(service Service, broker *events.Broker) Handler {
	handler := Handler{
		service: service,
		broker:  broker,
		tickets: NewSocketTickets(30 * time.Second),
	}
	handler.builds = NewBuildManager(service, handler.notifyProfilesChanged)
	return handler
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
	group.Get("/:id/bundles/:version", h.bundle)
	group.Get("/:id/objects/:hash", h.object)
	group.Get("/:id/files/*", h.download)

	// Браузерный WebSocket не умеет Authorization header. Админ сначала получает
	// одноразовый билет обычным защищённым POST, затем билет погашается до upgrade.
	app.Post("/api/admin/profiles/events-ticket", authMiddleware, auth.RequireAdmin, h.createEventsTicket)
	app.Get("/api/admin/profiles/ws", h.requireSocketTicket, websocket.New(h.eventsSocket, websocket.Config{
		HandshakeTimeout: 10 * time.Second,
		ReadBufferSize:   4096,
		WriteBufferSize:  4096,
	}))

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
	admin.Post("/:id/publish", h.scan)
	admin.Get("/:id/build", h.buildStatus)
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
	result, started, err := h.builds.Start(c.Context(), c.Params("id"))
	if err != nil {
		return h.writeError(c, err)
	}
	if !started {
		return c.Status(http.StatusOK).JSON(result)
	}
	return c.Status(http.StatusAccepted).JSON(result)
}

func (h Handler) buildStatus(c fiber.Ctx) error {
	result, ok := h.builds.Snapshot(c.Params("id"))
	if !ok {
		return h.writeError(c, gorm.ErrRecordNotFound)
	}
	return c.JSON(result)
}

func (h Handler) createEventsTicket(c fiber.Ctx) error {
	ticket, expiresAt, err := h.tickets.Issue()
	if err != nil {
		return c.Status(http.StatusInternalServerError).JSON(ErrorResponse{Message: "Не удалось открыть WebSocket"})
	}
	return c.JSON(fiber.Map{"ticket": ticket, "expiresAt": expiresAt})
}

func (h Handler) requireSocketTicket(c fiber.Ctx) error {
	if !websocket.IsWebSocketUpgrade(c) {
		return c.Status(http.StatusUpgradeRequired).JSON(ErrorResponse{Message: "Требуется WebSocket upgrade"})
	}
	if err := h.tickets.Consume(c.Query("ticket")); err != nil {
		return c.Status(http.StatusUnauthorized).JSON(ErrorResponse{Message: "WebSocket-билет недействителен"})
	}
	return c.Next()
}

type socketEnvelope struct {
	Type   string          `json:"type"`
	Event  string          `json:"event,omitempty"`
	Build  *BuildSnapshot  `json:"build,omitempty"`
	Builds []BuildSnapshot `json:"builds,omitempty"`
}

func (h Handler) eventsSocket(conn *websocket.Conn) {
	buildSubID, buildCh := h.builds.Subscribe()
	defer h.builds.Unsubscribe(buildSubID)

	var brokerSubID int
	var brokerCh <-chan string
	if h.broker != nil {
		brokerSubID, brokerCh = h.broker.Subscribe()
		defer h.broker.Unsubscribe(brokerSubID)
	}

	if err := conn.WriteJSON(socketEnvelope{Type: "manifest.snapshot", Builds: h.builds.Snapshots()}); err != nil {
		return
	}

	done := make(chan struct{})
	go func() {
		defer close(done)
		conn.SetReadLimit(1024)
		for {
			if _, _, err := conn.ReadMessage(); err != nil {
				return
			}
		}
	}()

	heartbeat := time.NewTicker(20 * time.Second)
	defer heartbeat.Stop()
	for {
		select {
		case <-done:
			return
		case build, ok := <-buildCh:
			if !ok {
				return
			}
			if err := conn.WriteJSON(socketEnvelope{Type: "manifest.build", Build: &build}); err != nil {
				return
			}
		case event, ok := <-brokerCh:
			if !ok {
				brokerCh = nil
				continue
			}
			if err := conn.WriteJSON(socketEnvelope{Type: "system.event", Event: event}); err != nil {
				return
			}
		case <-heartbeat.C:
			if err := conn.WriteMessage(websocket.PingMessage, nil); err != nil {
				return
			}
		}
	}
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
	etag, err := h.service.ManifestETag(c.Context(), c.Params("id"))
	if err != nil {
		return h.writeError(c, err)
	}
	c.Set(fiber.HeaderETag, etag)
	c.Set(fiber.HeaderCacheControl, "private, no-cache")
	if requestETagMatches(c.Get(fiber.HeaderIfNoneMatch), etag) {
		return c.SendStatus(http.StatusNotModified)
	}
	manifest, err := h.service.Manifest(c.Context(), c.Params("id"))
	if err != nil {
		return h.writeError(c, err)
	}
	return c.JSON(manifest)
}

func requestETagMatches(header, current string) bool {
	for _, candidate := range strings.Split(header, ",") {
		candidate = strings.TrimSpace(candidate)
		if candidate == "*" || candidate == current || strings.TrimPrefix(candidate, "W/") == current {
			return true
		}
	}
	return false
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

func (h Handler) bundle(c fiber.Ctx) error {
	version, err := strconv.Atoi(c.Params("version"))
	if err != nil || version <= 0 {
		return c.Status(http.StatusBadRequest).JSON(ErrorResponse{Message: "Некорректная версия сборки"})
	}
	download, err := h.service.Bundle(c.Context(), c.Params("id"), version)
	if err != nil {
		return h.writeError(c, err)
	}
	c.Set(fiber.HeaderContentDisposition, "attachment; filename=\"profile-"+c.Params("version")+".tar.zst\"")
	c.Set(fiber.HeaderCacheControl, "private, max-age=31536000, immutable")
	c.Set(fiber.HeaderETag, `"`+download.ETag+`"`)
	c.Set("Accept-Ranges", "bytes")
	if rangeHeader := c.Get(fiber.HeaderRange); rangeHeader != "" {
		return sendFileRange(c, download.AbsolutePath, download.Size, rangeHeader)
	}
	return c.SendFile(download.AbsolutePath)
}

func (h Handler) object(c fiber.Ctx) error {
	download, err := h.service.Object(c.Context(), c.Params("id"), c.Params("hash"))
	if err != nil {
		return h.writeError(c, err)
	}
	c.Set(fiber.HeaderCacheControl, "private, max-age=31536000, immutable")
	c.Set(fiber.HeaderETag, `"`+download.ETag+`"`)
	c.Set("Accept-Ranges", "bytes")
	if rangeHeader := c.Get(fiber.HeaderRange); rangeHeader != "" {
		return sendFileRange(c, download.AbsolutePath, download.Size, rangeHeader)
	}
	return c.SendFile(download.AbsolutePath)
}

func sendFileRange(c fiber.Ctx, path string, size int64, header string) error {
	if !strings.HasPrefix(header, "bytes=") || strings.Contains(header, ",") {
		c.Set(fiber.HeaderContentRange, fmt.Sprintf("bytes */%d", size))
		return c.SendStatus(http.StatusRequestedRangeNotSatisfiable)
	}
	parts := strings.SplitN(strings.TrimPrefix(header, "bytes="), "-", 2)
	if len(parts) != 2 || parts[0] == "" {
		c.Set(fiber.HeaderContentRange, fmt.Sprintf("bytes */%d", size))
		return c.SendStatus(http.StatusRequestedRangeNotSatisfiable)
	}
	start, err := strconv.ParseInt(parts[0], 10, 64)
	if err != nil || start < 0 || start >= size {
		c.Set(fiber.HeaderContentRange, fmt.Sprintf("bytes */%d", size))
		return c.SendStatus(http.StatusRequestedRangeNotSatisfiable)
	}
	end := size - 1
	if parts[1] != "" {
		end, err = strconv.ParseInt(parts[1], 10, 64)
		if err != nil || end < start {
			c.Set(fiber.HeaderContentRange, fmt.Sprintf("bytes */%d", size))
			return c.SendStatus(http.StatusRequestedRangeNotSatisfiable)
		}
		if end >= size {
			end = size - 1
		}
	}
	file, err := os.Open(path)
	if err != nil {
		return err
	}
	if _, err := file.Seek(start, io.SeekStart); err != nil {
		file.Close()
		return err
	}
	length := end - start + 1
	c.Status(http.StatusPartialContent)
	c.Set(fiber.HeaderContentRange, fmt.Sprintf("bytes %d-%d/%d", start, end, size))
	c.Set(fiber.HeaderContentLength, strconv.FormatInt(length, 10))
	return c.SendStream(&rangeReadCloser{Reader: io.LimitReader(file, length), Closer: file}, int(length))
}

type rangeReadCloser struct {
	io.Reader
	io.Closer
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
