package yggdrasil

import (
	"net/http"
	"strings"

	"launcher-backend/internal/auth"

	"github.com/gofiber/fiber/v3"
)

type Handler struct {
	service *Service
}

func NewHandler(service *Service) Handler {
	return Handler{service: service}
}

// yggError — стандартный формат ошибки Yggdrasil.
type yggError struct {
	Error        string `json:"error"`
	ErrorMessage string `json:"errorMessage"`
}

func forbidden(c fiber.Ctx, message string) error {
	return c.Status(http.StatusForbidden).JSON(yggError{
		Error:        "ForbiddenOperationException",
		ErrorMessage: message,
	})
}

// yggdrasilBasePaths — пути, под которыми доступен один и тот же Yggdrasil API.
// Второй — GML-совместимый, чтобы стоковый authlib-injector работал drop-in:
// на сервере достаточно сменить только хост в строке -javaagent.
var yggdrasilBasePaths = []string{
	"/api/yggdrasil",
	"/api/v1/integrations/authlib/minecraft",
}

// RegisterRoutes монтирует публичный Yggdrasil API (на него указывает javaagent)
// и защищённый JWT эндпоинт выпуска игровой сессии для лаунчера.
func (h Handler) RegisterRoutes(app *fiber.App, authMiddleware fiber.Handler) {
	for _, base := range yggdrasilBasePaths {
		h.mountYggdrasil(app.Group(base))
	}

	// Защищённый: лаунчер обменивает свой JWT на игровую сессию (Minecraft accessToken).
	app.Post("/api/yggdrasil/launcher-session", authMiddleware, h.launcherSession)

	// Защищённый keepalive: лаунчер продлевает сессию по nonce, пока процесс игры жив.
	// Надёжный сигнал живости вместо хрупкого heartbeat-треда агента (тот мог тихо
	// умереть в модовом окружении → сессия гасла reaper'ом → «Недействительная сессия»).
	app.Post("/api/yggdrasil/launcher-session/keepalive", authMiddleware, h.launcherSessionKeepalive)

	// Раздача самого agent-jar: лаунчер качает его в служебную папку и инжектит.
	app.Get("/api/yggdrasil/authlib-injector.jar", h.injectorJar)
}

func (h Handler) injectorJar(c fiber.Ctx) error {
	path := h.service.InjectorPath()
	if path == "" {
		return c.SendStatus(http.StatusNotFound)
	}
	c.Set(fiber.HeaderContentType, "application/java-archive")
	return c.SendFile(path)
}

// mountYggdrasil вешает весь набор Yggdrasil-эндпоинтов на переданную группу.
func (h Handler) mountYggdrasil(root fiber.Router) {
	root.Get("/", h.meta)

	root.Post("/authserver/authenticate", h.authenticate)
	root.Post("/authserver/refresh", h.refresh)
	root.Post("/authserver/validate", h.validate)
	root.Post("/authserver/invalidate", h.invalidate)
	root.Post("/authserver/signout", h.signout)

	root.Post("/sessionserver/session/minecraft/join", h.join)
	root.Get("/sessionserver/session/minecraft/hasJoined", h.hasJoined)
	root.Get("/sessionserver/session/minecraft/profile/:uuid", h.profile)

	root.Post("/api/profiles/minecraft", h.profilesByNames)
}

func (h Handler) meta(c fiber.Ctx) error {
	return c.JSON(h.service.Meta())
}

type launcherSessionRequest struct {
	Nonce string `json:"nonce"`
}

// launcherSession выдаёт authenticated-пользователю игровую сессию. Тело может
// содержать nonce из античит-handshake/init — он связывает сессию с launch-token,
// чтобы последующий confirm пометил её Verified (без этого join будет отклонён).
func (h Handler) launcherSession(c fiber.Ctx) error {
	user, ok := auth.CurrentUser(c)
	if !ok {
		return c.Status(http.StatusUnauthorized).JSON(yggError{Error: "Unauthorized", ErrorMessage: "Требуется авторизация"})
	}
	var req launcherSessionRequest
	_ = c.Bind().Body(&req)
	sess := h.service.IssueSession(user, req.Nonce)
	return c.JSON(fiber.Map{
		"accessToken": sess.AccessToken,
		"clientToken": sess.ClientToken,
		"uuid":        sess.UUID,
		"name":        sess.Name,
	})
}

type keepaliveRequest struct {
	Nonce string `json:"nonce"`
}

// launcherSessionKeepalive продлевает игровую сессию по nonce (sliding TTL). Лаунчер
// пингует, пока процесс игры жив; по nonce, а не accessToken, чтобы продление пережило
// /authserver/refresh (authlib-injector может сменить токен). No-op на неизвестной/
// истёкшей сессии (404) — истёкшую/погашенную не воскрешаем, kick остаётся окончательным.
func (h Handler) launcherSessionKeepalive(c fiber.Ctx) error {
	var req keepaliveRequest
	if err := c.Bind().Body(&req); err != nil || req.Nonce == "" {
		return c.Status(http.StatusBadRequest).JSON(yggError{Error: "IllegalArgumentException", ErrorMessage: "Не указан nonce"})
	}
	if !h.service.Store().TouchByNonce(req.Nonce) {
		return c.SendStatus(http.StatusNotFound)
	}
	// Фиксируем живость лаунчера: по ней anticheat-reaper отличает убийство агента в
	// живой игре (алерт) от обычного закрытия игры (тишина).
	h.service.Store().RecordLauncherKeepalive(req.Nonce)
	return c.SendStatus(http.StatusNoContent)
}

func (h Handler) authenticate(c fiber.Ctx) error {
	// Прямой логин по паролю не поддерживаем — сессию выдаёт лаунчер по JWT.
	return forbidden(c, "Авторизация выполняется через лаунчер Project Minecraft")
}

type tokenRequest struct {
	AccessToken     string         `json:"accessToken"`
	ClientToken     string         `json:"clientToken"`
	SelectedProfile map[string]any `json:"selectedProfile"`
	RequestUser     bool           `json:"requestUser"`
}

func (h Handler) refresh(c fiber.Ctx) error {
	var req tokenRequest
	if err := c.Bind().Body(&req); err != nil {
		return forbidden(c, "Некорректный запрос")
	}
	newToken := randomToken()
	sess, ok := h.service.Store().ReplaceToken(req.AccessToken, newToken)
	if !ok {
		return forbidden(c, "Недействительный токен")
	}
	return c.JSON(fiber.Map{
		"accessToken": sess.AccessToken,
		"clientToken": firstNonEmpty(req.ClientToken, sess.ClientToken),
		"selectedProfile": fiber.Map{
			"id":   sess.UUID,
			"name": sess.Name,
		},
	})
}

func (h Handler) validate(c fiber.Ctx) error {
	var req tokenRequest
	if err := c.Bind().Body(&req); err != nil {
		return forbidden(c, "Некорректный запрос")
	}
	if _, ok := h.service.Store().Session(req.AccessToken); !ok {
		return forbidden(c, "Недействительный токен")
	}
	return c.SendStatus(http.StatusNoContent)
}

func (h Handler) invalidate(c fiber.Ctx) error {
	var req tokenRequest
	if err := c.Bind().Body(&req); err == nil && req.AccessToken != "" {
		h.service.Store().Invalidate(req.AccessToken)
	}
	return c.SendStatus(http.StatusNoContent)
}

func (h Handler) signout(c fiber.Ctx) error {
	return c.SendStatus(http.StatusNoContent)
}

type joinRequest struct {
	AccessToken     string `json:"accessToken"`
	SelectedProfile string `json:"selectedProfile"`
	ServerID        string `json:"serverId"`
}

// join вызывается клиентом перед подключением: фиксируем serverId ↔ профиль,
// если accessToken валиден. Это и отличает игрока из лаунчера от пирата.
func (h Handler) join(c fiber.Ctx) error {
	var req joinRequest
	if err := c.Bind().Body(&req); err != nil {
		return forbidden(c, "Некорректный запрос")
	}
	sess, ok := h.service.Store().Session(req.AccessToken)
	if !ok {
		return forbidden(c, "Недействительный токен — запустите игру через лаунчер")
	}
	// Рычаг принуждения: сессия должна пройти античит-handshake (confirm от агентов).
	// Без verified — запуск без защиты или пропатченный лаунчер → доступ закрыт.
	if !sess.Verified {
		return forbidden(c, "Защита не подтверждена — запустите игру через лаунчер без модификаций")
	}
	if normalizeHex(req.SelectedProfile) != sess.UUID {
		return forbidden(c, "Профиль не совпадает с токеном")
	}
	h.service.Store().PutJoin(req.ServerID, JoinRecord{
		UUID: sess.UUID,
		Name: sess.Name,
		IP:   c.IP(),
	})
	// Sliding TTL: активная игра (вкл. переподключения) держит токен живым.
	h.service.Store().TouchSession(req.AccessToken)
	return c.SendStatus(http.StatusNoContent)
}

// hasJoined вызывает сервер: подтверждаем, что игрок только что прошёл /join.
// Нет записи (или ник не тот) — 204, и сервер отклоняет подключение.
func (h Handler) hasJoined(c fiber.Ctx) error {
	username := c.Query("username")
	serverID := c.Query("serverId")

	record, ok := h.service.Store().ConsumeJoin(serverID)
	if !ok || !strings.EqualFold(record.Name, username) {
		return c.SendStatus(http.StatusNoContent)
	}
	return c.JSON(h.service.profileFor(record.UUID, record.Name))
}

func (h Handler) profile(c fiber.Ctx) error {
	uuid := c.Params("uuid")
	prof, ok := h.service.LookupByUUID(c.Context(), uuid)
	if !ok {
		return c.SendStatus(http.StatusNoContent)
	}
	return c.JSON(prof)
}

func (h Handler) profilesByNames(c fiber.Ctx) error {
	var names []string
	if err := c.Bind().Body(&names); err != nil {
		return c.Status(http.StatusBadRequest).JSON(yggError{Error: "IllegalArgumentException", ErrorMessage: "Некорректный запрос"})
	}
	return c.JSON(h.service.LookupByNames(c.Context(), names))
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if value != "" {
			return value
		}
	}
	return ""
}
