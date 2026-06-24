package anticheat

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"strconv"
	"time"

	"launcher-backend/internal/auth"
	"launcher-backend/internal/launcherrelease"
	"launcher-backend/internal/models"
	"launcher-backend/internal/yggdrasil"

	"github.com/gofiber/fiber/v3"
)

// VersionGate сообщает минимальную обязательную версию лаунчера
// (реализуется launcherrelease.Service). nil — форс-апдейт выключен.
type VersionGate interface {
	MinMandatoryVersion(ctx context.Context) (string, error)
}

type Handler struct {
	service     *Service
	versionGate VersionGate
	// Лимитеры (указатели — переживают копирование Handler в WithVersionGate).
	initLimiter   *rateLimiter
	detectLimiter *rateLimiter
	hbLimiter     *rateLimiter
}

type ErrorResponse struct {
	Message string `json:"message"`
}

func NewHandler(service *Service) Handler {
	return Handler{
		service:       service,
		initLimiter:   newRateLimiter(10, time.Minute),
		detectLimiter: newRateLimiter(40, time.Minute),
		hbLimiter:     newRateLimiter(6, time.Minute),
	}
}

// WithVersionGate включает серверный форс-апдейт: клиенты ниже минимальной
// обязательной версии не получают launch-token (426 Upgrade Required).
func (h Handler) WithVersionGate(gate VersionGate) Handler {
	h.versionGate = gate
	return h
}

// RegisterRoutes монтирует игровые (JWT/launch-token) и admin-эндпоинты античита.
func (h Handler) RegisterRoutes(app *fiber.App, authMiddleware fiber.Handler) {
	// Авторизация навешивается per-route: group.Use(...) применялась бы по префиксу
	// ко всем роутам /api/anticheat, включая launch-token-эндпоинты (им JWT не нужен).
	group := app.Group("/api/anticheat")
	// JWT-защищённые: лаунчер инициирует handshake и тянет блэклист.
	group.Post("/handshake/init", authMiddleware, h.init)
	group.Get("/blacklist", authMiddleware, h.blacklist)
	// Launch-token-защищённые: confirm и репорты от лаунчера/агентов (без JWT).
	group.Post("/handshake/confirm", h.confirm)
	group.Post("/detect", h.detect)
	group.Post("/heartbeat", h.heartbeat)
	// Лёгкая телеметрия агента (по launch-token): агент сообщает о самовосстановлении
	// своих фоновых тредов (heartbeat/event-poller пережили interrupt/Throwable).
	// Только лог — ни БД, ни бана, ни алерта.
	group.Post("/diag", h.diag)
	// Блэклист для агента (без JWT, по launch-token): версия + сигнатуры для рантайм-скана.
	group.Get("/rules", h.rules)
	// Раздача agent.jar: лаунчер качает его и инжектит как -javaagent.
	group.Get("/agent.jar", h.agentJar)
	// Раздача нативной JVMTI-библиотеки по ОС: лаунчер инжектит как -agentpath.
	group.Get("/native/:os", h.nativeLib)
	// Манифест целостности (SHA-256 артефактов): лаунчер сверяет скачанное перед инжектом.
	group.Get("/manifest", h.manifest)

	// Admin: просмотр и управление.
	admin := app.Group("/api/admin/anticheat")
	admin.Use(authMiddleware, auth.RequireAdmin)
	admin.Get("/detections", h.listDetections)
	admin.Patch("/detections/:id", h.updateDetectionStatus)
	admin.Get("/stats", h.signatureStats)
	admin.Get("/bans/hwid", h.listHwidBans)
	admin.Get("/bans/account", h.listAccountBans)
	admin.Post("/bans/hwid", h.banHwid)
	admin.Post("/bans/account", h.banAccount)
	admin.Delete("/bans/hwid/:hash", h.unbanHwid)
	admin.Delete("/bans/account/:uuid", h.unbanAccount)
	admin.Get("/signatures", h.listSignatures)
	admin.Post("/signatures", h.createSignature)
	admin.Patch("/signatures/:id", h.updateSignature)
	admin.Delete("/signatures/:id", h.deleteSignature)
}

type initRequest struct {
	HwidHash       string           `json:"hwidHash"`
	HwidComponents HwidComponents   `json:"hwidComponents"`
	Detections     []DetectionInput `json:"detections"`
}

func (h Handler) init(c fiber.Ctx) error {
	// Форс-апдейт: старый лаунчер не получает launch-token, пока не обновится.
	// Запрос без заголовка — легаси-версия (≤0.1.0), считается "0.0.0".
	if h.versionGate != nil {
		minVersion, err := h.versionGate.MinMandatoryVersion(c.Context())
		if err != nil {
			// fail-open: при сбое БД гейт не блокирует игроков, но сбой должен быть виден.
			slog.Warn("anticheat: version gate degraded (fail-open)", "error", err)
		}
		if err == nil && minVersion != "" {
			clientVersion := c.Get("X-Launcher-Version")
			if clientVersion == "" {
				clientVersion = "0.0.0"
			}
			if launcherrelease.CompareVersions(clientVersion, minVersion) < 0 {
				return c.Status(http.StatusUpgradeRequired).JSON(ErrorResponse{
					Message: "Требуется обновление лаунчера до версии " + minVersion,
				})
			}
		}
	}

	user, ok := auth.CurrentUser(c)
	if !ok {
		return c.Status(http.StatusUnauthorized).JSON(ErrorResponse{Message: "Требуется авторизация"})
	}
	if !h.initLimiter.allow(user.ID) {
		return c.Status(http.StatusTooManyRequests).JSON(ErrorResponse{Message: "Слишком много запросов"})
	}
	var req initRequest
	if err := c.Bind().Body(&req); err != nil {
		return c.Status(http.StatusBadRequest).JSON(ErrorResponse{Message: "Некорректный запрос"})
	}
	userUUID := yggdrasil.NormalizeUUID(user.ProviderUUID, user.Login)
	result, err := h.service.InitHandshakeWithComponents(c.Context(), userUUID, user.Login, req.HwidHash, req.HwidComponents, req.Detections)
	if err != nil {
		return c.Status(http.StatusInternalServerError).JSON(ErrorResponse{Message: "Ошибка инициализации"})
	}
	if !result.Allowed {
		// Блок запуска: лаунчер не должен стартовать игру.
		return c.Status(http.StatusForbidden).JSON(result)
	}
	return c.JSON(result)
}

type confirmRequest struct {
	LaunchToken string       `json:"launchToken"`
	Proof       ConfirmProof `json:"proof"`
}

func (h Handler) confirm(c fiber.Ctx) error {
	var req confirmRequest
	if err := c.Bind().Body(&req); err != nil {
		return c.Status(http.StatusBadRequest).JSON(ErrorResponse{Message: "Некорректный запрос"})
	}
	token := req.LaunchToken
	if token == "" {
		token = c.Get("X-Launch-Token")
	}
	if err := h.service.Confirm(token, req.Proof); err != nil {
		return c.Status(http.StatusUnauthorized).JSON(ErrorResponse{Message: "Не удалось подтвердить защиту"})
	}
	return c.SendStatus(http.StatusNoContent)
}

type detectRequest struct {
	LaunchToken string         `json:"launchToken"`
	Source      string         `json:"source"`
	Type        string         `json:"type"`
	Signature   string         `json:"signature"`
	Severity    int            `json:"severity"`
	Details     map[string]any `json:"details"`
}

func (h Handler) detect(c fiber.Ctx) error {
	var req detectRequest
	if err := c.Bind().Body(&req); err != nil {
		return c.Status(http.StatusBadRequest).JSON(ErrorResponse{Message: "Некорректный запрос"})
	}
	token := req.LaunchToken
	if token == "" {
		token = c.Get("X-Launch-Token")
	}
	claims, err := h.service.VerifyToken(token)
	if err != nil {
		return c.Status(http.StatusUnauthorized).JSON(ErrorResponse{Message: "Недействительный токен сессии"})
	}
	if !h.detectLimiter.allow(claims.UUID) {
		return c.Status(http.StatusTooManyRequests).JSON(ErrorResponse{Message: "Слишком много запросов"})
	}
	input := DetectionInput{
		Source:    req.Source,
		Type:      req.Type,
		Signature: req.Signature,
		Severity:  req.Severity, // игнорируется сервером — severity вычисляется в RecordDetection
		Details:   req.Details,
	}
	severity, confidence, err := h.service.RecordDetection(c.Context(), claims, input)
	if err != nil {
		return c.Status(http.StatusInternalServerError).JSON(ErrorResponse{Message: "Не удалось записать детект"})
	}
	// Решаем, кикать ли игрока (по СЕРВЕРНЫМ severity+confidence): ответ читает агент и убивает JVM.
	if kick, reason := h.service.EvaluateKick(claims, severity, confidence, input.Type); kick {
		return c.JSON(fiber.Map{"action": "kick", "reason": reason})
	}
	return c.JSON(fiber.Map{"action": "none"})
}

// heartbeat — периодический сигнал от Java-агента (launch-token). В M3 лишь
// подтверждает валидность токена; проверка свежести для realtime-kick — в M5.
func (h Handler) heartbeat(c fiber.Ctx) error {
	var req struct {
		LaunchToken string `json:"launchToken"`
	}
	_ = c.Bind().Body(&req)
	token := req.LaunchToken
	if token == "" {
		token = c.Get("X-Launch-Token")
	}
	claims, err := h.service.VerifyToken(token)
	if err != nil {
		return c.Status(http.StatusUnauthorized).JSON(ErrorResponse{Message: "Недействительный токен сессии"})
	}
	if !h.hbLimiter.allow(claims.UUID) {
		return c.Status(http.StatusTooManyRequests).JSON(ErrorResponse{Message: "Слишком много запросов"})
	}
	// kick=true, если сессию погасил detect; blacklistVersion — для ре-фетча правил агентом.
	kick, version := h.service.Heartbeat(c.Context(), claims)
	action := "none"
	if kick {
		action = "kick"
	}
	return c.JSON(fiber.Map{"action": action, "blacklistVersion": version})
}

// diag принимает телеметрию самовосстановления тредов агента. Назначение —
// диагностика: увидеть в логах прода, ЧТО прерывает фоновые треды агента в модовом
// окружении (механизм «Недействительной сессии»). Никакой бизнес-логики: только лог.
func (h Handler) diag(c fiber.Ctx) error {
	var req struct {
		LaunchToken string `json:"launchToken"`
		Event       string `json:"event"`
		Detail      string `json:"detail"`
	}
	_ = c.Bind().Body(&req)
	token := req.LaunchToken
	if token == "" {
		token = c.Get("X-Launch-Token")
	}
	claims, err := h.service.VerifyToken(token)
	if err != nil {
		return c.Status(http.StatusUnauthorized).JSON(ErrorResponse{Message: "Недействительный токен сессии"})
	}
	if !h.hbLimiter.allow(claims.UUID) {
		return c.Status(http.StatusTooManyRequests).JSON(ErrorResponse{Message: "Слишком много запросов"})
	}
	// Обрезаем поля, чтобы агент не мог залить логи произвольным объёмом.
	event, detail := truncate(req.Event, 64), truncate(req.Detail, 256)
	slog.Info("anticheat agent diag", "login", claims.Login, "uuid", claims.UUID,
		"event", event, "detail", detail)
	return c.SendStatus(http.StatusNoContent)
}

// truncate ограничивает длину строки телеметрии (защита логов от флуда).
func truncate(s string, max int) string {
	if len(s) > max {
		return s[:max]
	}
	return s
}

func (h Handler) rules(c fiber.Ctx) error {
	token := c.Get("X-Launch-Token")
	if token == "" {
		token = c.Query("launchToken")
	}
	if _, err := h.service.VerifyToken(token); err != nil {
		return c.Status(http.StatusUnauthorized).JSON(ErrorResponse{Message: "Недействительный токен сессии"})
	}
	rules, err := h.service.Rules(c.Context())
	if err != nil {
		return c.Status(http.StatusInternalServerError).JSON(ErrorResponse{Message: "Не удалось получить правила"})
	}
	return c.JSON(rules)
}

func (h Handler) agentJar(c fiber.Ctx) error {
	path := h.service.AgentPath()
	if path == "" {
		return c.SendStatus(http.StatusNotFound)
	}
	c.Set(fiber.HeaderContentType, "application/java-archive")
	return c.SendFile(path)
}

func (h Handler) nativeLib(c fiber.Ctx) error {
	path := h.service.NativePath(c.Params("os"))
	if path == "" {
		return c.SendStatus(http.StatusNotFound)
	}
	c.Set(fiber.HeaderContentType, "application/octet-stream")
	return c.SendFile(path)
}

func (h Handler) manifest(c fiber.Ctx) error {
	return c.JSON(h.service.Manifest())
}

func (h Handler) blacklist(c fiber.Ctx) error {
	// ETag по версии блэклиста: лаунчер с If-None-Match получит 304, если ничего не менялось.
	etag := fmt.Sprintf(`"ac-v%d"`, h.service.BlacklistVersion(c.Context()))
	if c.Get("If-None-Match") == etag {
		return c.SendStatus(http.StatusNotModified)
	}
	sigs, err := h.service.Blacklist(c.Context())
	if err != nil {
		return c.Status(http.StatusInternalServerError).JSON(ErrorResponse{Message: "Не удалось получить блэклист"})
	}
	c.Set("ETag", etag)
	return c.JSON(sigs)
}

// --- Admin ---

func (h Handler) listDetections(c fiber.Ctx) error {
	limit, _ := strconv.Atoi(c.Query("limit", ""))
	minSev, _ := strconv.Atoi(c.Query("minSeverity", ""))
	filter := DetectionFilter{
		Status:      c.Query("status", ""),
		Confidence:  c.Query("confidence", ""),
		MinSeverity: minSev,
	}
	items, err := h.service.ListDetections(c.Context(), limit, filter)
	if err != nil {
		return c.Status(http.StatusInternalServerError).JSON(ErrorResponse{Message: "Не удалось получить детекты"})
	}
	return c.JSON(items)
}

// updateDetectionStatus меняет статус разбора детекта в review-очереди (admin).
func (h Handler) updateDetectionStatus(c fiber.Ctx) error {
	admin, _ := auth.CurrentUser(c)
	var req struct {
		Status string `json:"status"`
	}
	if err := c.Bind().Body(&req); err != nil || req.Status == "" {
		return c.Status(http.StatusBadRequest).JSON(ErrorResponse{Message: "Укажите status"})
	}
	if err := h.service.UpdateDetectionStatus(c.Context(), c.Params("id"), req.Status, admin.Login); err != nil {
		return c.Status(http.StatusBadRequest).JSON(ErrorResponse{Message: "Недопустимый статус или детект не найден"})
	}
	return c.SendStatus(http.StatusNoContent)
}

// signatureStats — агрегированная статистика детектов по сигнатурам за N дней (admin).
// Инструмент оценки false-positive rate перед включением авто-бана.
func (h Handler) signatureStats(c fiber.Ctx) error {
	days, _ := strconv.Atoi(c.Query("days", "7"))
	if days <= 0 {
		days = 7
	}
	since := time.Now().AddDate(0, 0, -days)
	stats, err := h.service.SignatureStats(c.Context(), since)
	if err != nil {
		return c.Status(http.StatusInternalServerError).JSON(ErrorResponse{Message: "Не удалось получить статистику"})
	}
	return c.JSON(stats)
}

func (h Handler) listHwidBans(c fiber.Ctx) error {
	items, err := h.service.ListHwidBans(c.Context())
	if err != nil {
		return c.Status(http.StatusInternalServerError).JSON(ErrorResponse{Message: "Ошибка"})
	}
	return c.JSON(items)
}

func (h Handler) listAccountBans(c fiber.Ctx) error {
	items, err := h.service.ListAccountBans(c.Context())
	if err != nil {
		return c.Status(http.StatusInternalServerError).JSON(ErrorResponse{Message: "Ошибка"})
	}
	return c.JSON(items)
}

type banHwidRequest struct {
	HwidHash string `json:"hwidHash"`
	Reason   string `json:"reason"`
}

func (h Handler) banHwid(c fiber.Ctx) error {
	user, _ := auth.CurrentUser(c)
	var req banHwidRequest
	if err := c.Bind().Body(&req); err != nil || req.HwidHash == "" {
		return c.Status(http.StatusBadRequest).JSON(ErrorResponse{Message: "Укажите hwidHash"})
	}
	if err := h.service.BanHwid(c.Context(), req.HwidHash, req.Reason, user.Login); err != nil {
		return c.Status(http.StatusInternalServerError).JSON(ErrorResponse{Message: "Не удалось забанить"})
	}
	return c.SendStatus(http.StatusNoContent)
}

type banAccountRequest struct {
	UserUUID string `json:"userUuid"`
	Login    string `json:"login"`
	Reason   string `json:"reason"`
}

func (h Handler) banAccount(c fiber.Ctx) error {
	admin, _ := auth.CurrentUser(c)
	var req banAccountRequest
	if err := c.Bind().Body(&req); err != nil || req.UserUUID == "" {
		return c.Status(http.StatusBadRequest).JSON(ErrorResponse{Message: "Укажите userUuid"})
	}
	if err := h.service.BanAccount(c.Context(), req.UserUUID, req.Login, req.Reason, admin.Login); err != nil {
		return c.Status(http.StatusInternalServerError).JSON(ErrorResponse{Message: "Не удалось забанить"})
	}
	return c.SendStatus(http.StatusNoContent)
}

func (h Handler) unbanHwid(c fiber.Ctx) error {
	if err := h.service.UnbanHwid(c.Context(), c.Params("hash")); err != nil {
		return c.Status(http.StatusInternalServerError).JSON(ErrorResponse{Message: "Ошибка"})
	}
	return c.SendStatus(http.StatusNoContent)
}

func (h Handler) unbanAccount(c fiber.Ctx) error {
	if err := h.service.UnbanAccount(c.Context(), c.Params("uuid")); err != nil {
		return c.Status(http.StatusInternalServerError).JSON(ErrorResponse{Message: "Ошибка"})
	}
	return c.SendStatus(http.StatusNoContent)
}

func (h Handler) listSignatures(c fiber.Ctx) error {
	items, err := h.service.ListSignatures(c.Context())
	if err != nil {
		return c.Status(http.StatusInternalServerError).JSON(ErrorResponse{Message: "Ошибка"})
	}
	return c.JSON(items)
}

func (h Handler) createSignature(c fiber.Ctx) error {
	var sig models.CheatSignature
	if err := c.Bind().Body(&sig); err != nil || sig.Kind == "" {
		return c.Status(http.StatusBadRequest).JSON(ErrorResponse{Message: "Укажите kind"})
	}
	created, err := h.service.CreateSignature(c.Context(), sig)
	if err != nil {
		return c.Status(http.StatusInternalServerError).JSON(ErrorResponse{Message: "Не удалось создать"})
	}
	return c.Status(http.StatusCreated).JSON(created)
}

func (h Handler) updateSignature(c fiber.Ctx) error {
	var updates map[string]any
	if err := c.Bind().Body(&updates); err != nil {
		return c.Status(http.StatusBadRequest).JSON(ErrorResponse{Message: "Некорректный запрос"})
	}
	delete(updates, "id")
	delete(updates, "createdAt")
	if err := h.service.UpdateSignature(c.Context(), c.Params("id"), normalizeSignatureUpdates(updates)); err != nil {
		return c.Status(http.StatusInternalServerError).JSON(ErrorResponse{Message: "Не удалось обновить"})
	}
	return c.SendStatus(http.StatusNoContent)
}

func (h Handler) deleteSignature(c fiber.Ctx) error {
	if err := h.service.DeleteSignature(c.Context(), c.Params("id")); err != nil {
		return c.Status(http.StatusInternalServerError).JSON(ErrorResponse{Message: "Ошибка"})
	}
	return c.SendStatus(http.StatusNoContent)
}

// normalizeSignatureUpdates переводит JSON-ключи (camelCase) в имена колонок GORM.
func normalizeSignatureUpdates(in map[string]any) map[string]any {
	mapping := map[string]string{
		"kind":      "kind",
		"pattern":   "pattern",
		"matchType": "match_type",
		"hashHex":   "hash_hex",
		"severity":  "severity",
		"note":      "note",
		"enabled":   "enabled",
	}
	out := make(map[string]any, len(in))
	for k, v := range in {
		if col, ok := mapping[k]; ok {
			out[col] = v
		}
	}
	return out
}
