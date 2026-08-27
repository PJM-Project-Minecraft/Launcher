package anticheat

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"

	"launcher-backend/internal/auth"
	"launcher-backend/internal/launcherrelease"
	"launcher-backend/internal/models"
	"launcher-backend/internal/policy"
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
	screenshots *ScreenshotService
	sessions    OnlineSessionsProvider
	versionGate VersionGate
	p5          p5Config
	// Лимитеры (указатели — переживают копирование Handler в WithVersionGate).
	initLimiter    *rateLimiter
	confirmLimiter *rateLimiter
	detectLimiter  *rateLimiter
	hbLimiter      *rateLimiter
	// Лимитер аплоада скриншотов (на UUID игрока): лаунчер грузит редко, но защита от сбоя.
	screenshotLimiter *rateLimiter
}

// OnlineSessionsProvider — доступ к живым игровым сессиям для скриншот-запросов и
// списка онлайн-игроков. Реализуется yggdrasil.Store; интерфейс развязывает пакеты.
type OnlineSessionsProvider interface {
	ActiveSessions() []yggdrasil.OnlineSession
	SessionByNonce(nonce string) (yggdrasil.Session, bool)
	// VerifiedSessionsByName — живые Verified-сессии игрока по нику (для P5-verify).
	VerifiedSessionsByName(name string) []yggdrasil.Session
}

func NewHandler(service *Service) Handler {
	return Handler{
		service:        service,
		initLimiter:    newRateLimiter(10, time.Minute),
		confirmLimiter: newRateLimiter(6, time.Minute),
		detectLimiter:  newRateLimiter(40, time.Minute),
		// 12, а не 6: heartbeat теперь делает до 2 попыток за цикл (промах на слабом
		// канале не должен стоить 30с тишины), плюс редкий diag с того же ключа.
		hbLimiter:         newRateLimiter(12, time.Minute),
		screenshotLimiter: newRateLimiter(10, time.Minute),
	}
}

// WithScreenshots подключает подсистему скриншотов и провайдера онлайн-сессий.
func (h Handler) WithScreenshots(ss *ScreenshotService, sessions OnlineSessionsProvider) Handler {
	h.screenshots = ss
	h.sessions = sessions
	return h
}

type ErrorResponse struct {
	Message string `json:"message"`
}

// WithVersionGate включает серверный форс-апдейт: клиенты ниже минимальной
// обязательной версии не получают launch-token (426 Upgrade Required).
func (h Handler) WithVersionGate(gate VersionGate) Handler {
	h.versionGate = gate
	return h
}

// WithP5 включает серверно-авторитетный handshake (P5): игровой NeoForge-сервер сверяет
// присутствие мода через /p5/verify. secret пуст — P5 выключен; enforce=false — репорт-онли.
func (h Handler) WithP5(secret string, enforce bool) Handler {
	h.p5 = p5Config{secret: secret, enforce: enforce}
	return h
}

// RegisterRoutes монтирует игровые (JWT/launch-token) и admin-эндпоинты античита.
func (h Handler) RegisterRoutes(app *fiber.App, authMiddleware fiber.Handler) {
	// Авторизация навешивается per-route: group.Use(...) применялась бы по префиксу
	// ко всем роутам /api/anticheat, включая launch-token-эндпоинты (им JWT не нужен).
	group := app.Group("/api/anticheat")
	// JWT-защищённые: лаунчер инициирует handshake и тянет блэклист.
	// initMaxBodySize, а не app-wide 512МБ: init принимает от клиента списки компонентов
	// и pre-launch детектов, каждый из которых оборачивается в запись БД.
	group.Post("/handshake/init", authMiddleware, requestBodyLimit(initMaxBodySize, h.init))
	group.Get("/blacklist", authMiddleware, h.blacklist)
	// Launch-token-защищённые: confirm и репорты от лаунчера/агентов (без JWT).
	group.Post("/handshake/confirm", h.confirm)
	group.Post("/detect", requestBodyLimit(detectMaxBodySize, h.detect))
	group.Post("/heartbeat", h.heartbeat)
	// Инвентарь mods/ от Java-агента (по launch-token): сервер сверяет SHA-256 со
	// сборками и решает, кикать ли за посторонний jar.
	group.Post("/files", requestBodyLimit(filesMaxBodySize, h.files))
	// Лёгкая телеметрия агента (по launch-token): агент сообщает о самовосстановлении
	// своих фоновых тредов (heartbeat/event-poller пережили interrupt/Throwable).
	// Только лог — ни БД, ни бана, ни алерта.
	group.Post("/diag", h.diag)
	// Блэклист для агента (без JWT, по launch-token): версия + сигнатуры для рантайм-скана.
	group.Get("/rules", h.rules)
	// P5: серверно-авторитетный handshake от игрового NeoForge-сервера. Аутентификация —
	// общий секрет ANTICHEAT_P5_SECRET в заголовке X-AC-P5-Secret (server-to-server, не JWT;
	// per-route, как остальные не-JWT роуты этой группы).
	group.Post("/p5/verify", h.p5Verify)
	group.Post("/p5/lease", h.p5Lease)
	// P5-опрос: игровой сервер спрашивает, кого из онлайна кикнуть (отзыв доступа —
	// молчание агента, detect-kick). Исполняет кик игровой сервер, не агент в JVM игрока.
	group.Post("/p5/revoked", h.p5Revoked)
	// Раздача agent.jar: лаунчер качает его и инжектит как -javaagent.
	group.Get("/agent.jar", h.agentJar)
	// Раздача нативной JVMTI-библиотеки по ОС: лаунчер инжектит как -agentpath.
	group.Get("/native/:os", h.nativeLib)
	// Манифест целостности (SHA-256 артефактов): лаунчер сверяет скачанное перед инжектом.
	// JWT-защищён: публичный agentSha256 позволял бы подделать attestation-proof (эхо
	// ожидаемого self-hash). Манифест тянется на этапе pre-launch (после логина, JWT уже
	// есть), поэтому закрытие не мешает лаунчеру. agent.jar/native остаются публичными —
	// их лаунчер качает тем же этапом, но по прямым раздачам без JWT (как и раньше).
	group.Get("/manifest", authMiddleware, h.manifest)

	// Скриншоты (по launch-token): лаунчер опрашивает pending-запрос, грузит JPEG
	// и сообщает об ошибке захвата. launch-token привязан к nonce онлайн-сессии,
	// поэтому JWT не нужен — токен уже аутентифицирует конкретного игрока на
	// конкретной игровой сессии. Токен берётся ТОЛЬКО из заголовка (не из query —
	// иначе утечёт в логи reverse-proxy). Per-route BodyLimit сужает app-wide 512МБ
	// до потолка скриншота на upload (защита memory-DoS).
	group.Get("/screenshot/pending", h.screenshotPending)
	group.Post("/screenshot/:id", screenshotUploadBodyLimit(h.screenshotUpload))
	group.Post("/screenshot/:id/fail", h.screenshotFail)

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

	// Скриншоты (admin): список онлайн-игроков, запрос скриншота, список/просмотр.
	if h.screenshots != nil && h.sessions != nil {
		admin.Get("/sessions/online", h.listOnlineSessions)
		admin.Post("/screenshots", h.requestScreenshot)
		admin.Get("/screenshots", h.listScreenshots)
		admin.Get("/screenshots/:id/image", h.screenshotImage)
	}
}

type initRequest struct {
	HwidHash       string           `json:"hwidHash"`
	HwidComponents HwidComponents   `json:"hwidComponents"`
	Detections     []DetectionInput `json:"detections"`
}

const (
	// maxInitDetections — потолок pre-launch детектов в одном init.
	maxInitDetections = 32
	// maxHwidMacs — потолок хешей MAC-адресов в компонентах HWID.
	maxHwidMacs = 16
	// hwidHashLen — длина солёного SHA-256 в hex (формат всех хешей HWID лаунчера,
	// src-tauri/src/anticheat/hwid.rs).
	hwidHashLen = 64
)

// validHwidHash — пусто (клиент не собрал отпечаток) либо 64 hex-символа.
// Проверка формата НЕ делает HWID доверенным (его считает клиент, и подделать его
// может любой), но не даёт засорять таблицу HWID и баны значениями вроде "1".
func validHwidHash(s string) bool {
	if s == "" {
		return true
	}
	if len(s) != hwidHashLen {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return false
		}
	}
	return true
}

func validHwidComponents(comps HwidComponents) bool {
	if !validHwidHash(comps.MachineID) || !validHwidHash(comps.BoardUUID) {
		return false
	}
	if len(comps.Macs) > maxHwidMacs {
		return false
	}
	for _, m := range comps.Macs {
		if !validHwidHash(m) {
			return false
		}
	}
	return true
}

func (h Handler) init(c fiber.Ctx) error {
	clientVersion := c.Get("X-Launcher-Version")
	if clientVersion == "" {
		clientVersion = "0.0.0"
	}
	// Форс-апдейт: старый лаунчер не получает launch-token, пока не обновится.
	// Запрос без заголовка — легаси-версия (≤0.1.0), считается "0.0.0".
	if h.versionGate != nil {
		minVersion, err := h.versionGate.MinMandatoryVersion(c.Context())
		if err != nil {
			// fail-open: при сбое БД гейт не блокирует игроков, но сбой должен быть виден.
			slog.Warn("anticheat: version gate degraded (fail-open)", "error", err)
		}
		if err == nil && minVersion != "" {
			if launcherUpdateRequired(clientVersion, minVersion) {
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
	// Юридический гейт: без принятой актуальной Политики конфиденциальности
	// launch-token не выдаётся. 451 Unavailable For Legal Reasons.
	if policy.NeedsConsent(&user) {
		return c.Status(http.StatusUnavailableForLegalReasons).JSON(fiber.Map{
			"code":    "policy_required",
			"message": "Примите Политику конфиденциальности: выйдите из аккаунта в лаунчере и войдите снова.",
		})
	}
	if !h.initLimiter.allow(user.ID) {
		return c.Status(http.StatusTooManyRequests).JSON(ErrorResponse{Message: "Слишком много запросов"})
	}
	var req initRequest
	if err := c.Bind().Body(&req); err != nil {
		return c.Status(http.StatusBadRequest).JSON(ErrorResponse{Message: "Некорректный запрос"})
	}
	if !validHwidHash(req.HwidHash) || !validHwidComponents(req.HwidComponents) {
		return c.Status(http.StatusBadRequest).JSON(ErrorResponse{Message: "Некорректный HWID"})
	}
	// Потолок pre-launch детектов: тело init не ограничено BodyLimit'ом, и поддельный
	// клиент иначе заливает в БД сколько угодно записей одним запросом.
	if len(req.Detections) > maxInitDetections {
		req.Detections = req.Detections[:maxInitDetections]
	}
	userUUID := yggdrasil.NormalizeUUID(user.ProviderUUID, user.Login)
	result, err := h.service.InitHandshakeWithVersion(
		c.Context(), userUUID, user.Login, req.HwidHash, req.HwidComponents,
		req.Detections, clientVersion,
	)
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
	token := launchTokenFromBody(c, req.LaunchToken)
	// Проверяем токен ДО Confirm, чтобы лимитировать по UUID: без этого confirm был
	// единственным нелимитированным launch-token-роутом → флуд алертов + рост goroutine
	// (в transition-режиме каждый невалидный proof порождает go NotifyDetection).
	claims, err := h.service.VerifyToken(token)
	if err != nil {
		return c.Status(http.StatusUnauthorized).JSON(ErrorResponse{Message: "Не удалось подтвердить защиту"})
	}
	if !h.confirmLimiter.allow(claims.UUID) {
		return c.Status(http.StatusTooManyRequests).JSON(ErrorResponse{Message: "Слишком много запросов"})
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
	claims, err := h.service.VerifySessionToken(launchTokenFromBody(c, req.LaunchToken))
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

type filesRequest struct {
	LaunchToken string         `json:"launchToken"`
	Files       []ReportedFile `json:"files"`
}

// files принимает список jar-ов из mods/ игрока (путь + SHA-256) и отвечает kick'ом,
// если среди них есть файл, которого нет ни в одной сборке.
func (h Handler) files(c fiber.Ctx) error {
	var req filesRequest
	if err := c.Bind().Body(&req); err != nil {
		return c.Status(http.StatusBadRequest).JSON(ErrorResponse{Message: "Некорректный запрос"})
	}
	claims, err := h.service.VerifySessionToken(launchTokenFromBody(c, req.LaunchToken))
	if err != nil {
		return c.Status(http.StatusUnauthorized).JSON(ErrorResponse{Message: "Недействительный токен сессии"})
	}
	if !h.detectLimiter.allow(claims.UUID) {
		return c.Status(http.StatusTooManyRequests).JSON(ErrorResponse{Message: "Слишком много запросов"})
	}
	if len(req.Files) > maxReportedFiles {
		req.Files = req.Files[:maxReportedFiles]
	}
	kick, err := h.service.CheckFiles(c.Context(), claims, req.Files)
	if err != nil {
		return c.Status(http.StatusInternalServerError).JSON(ErrorResponse{Message: "Не удалось проверить файлы"})
	}
	if kick {
		return c.JSON(fiber.Map{"action": "kick", "reason": unknownModType})
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
	claims, err := h.service.VerifySessionToken(launchTokenFromBody(c, req.LaunchToken))
	if err != nil {
		return c.Status(http.StatusUnauthorized).JSON(ErrorResponse{Message: "Недействительный токен сессии"})
	}
	if !h.hbLimiter.allow(claims.UUID) {
		return c.Status(http.StatusTooManyRequests).JSON(ErrorResponse{Message: "Слишком много запросов"})
	}
	// kick=true, если сессию погасил detect; blacklistVersion — для ре-фетча правил агентом.
	kick, version := h.service.Heartbeat(c.Context(), claims)
	reason := ""
	// Обязательный релиз мог появиться уже после запуска Minecraft. Проверяем его на
	// каждом heartbeat: старый лаунчер получает action=kick, сессия инвалидируется,
	// а P5-мод кикнет игрока даже если Java-агент был остановлен.
	if !kick && h.versionGate != nil {
		minVersion, gateErr := h.versionGate.MinMandatoryVersion(c.Context())
		if gateErr != nil {
			slog.Warn("anticheat: heartbeat version gate degraded (fail-open)", "error", gateErr)
		} else if launcherUpdateRequired(claims.LauncherVersion, minVersion) {
			h.service.KickForLauncherUpdate(claims, minVersion)
			kick = true
			reason = "launcher_update"
		}
	}
	action := "none"
	if kick {
		action = "kick"
	}
	return c.JSON(fiber.Map{"action": action, "reason": reason, "blacklistVersion": version})
}

func launcherUpdateRequired(clientVersion, minVersion string) bool {
	if minVersion == "" {
		return false
	}
	if clientVersion == "" {
		clientVersion = "0.0.0"
	}
	return launcherrelease.CompareVersions(clientVersion, minVersion) < 0
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
	claims, err := h.service.VerifySessionToken(launchTokenFromBody(c, req.LaunchToken))
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
	if _, err := h.service.VerifySessionToken(launchTokenFromHeader(c)); err != nil {
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

// --- Скриншоты ---

// listOnlineSessions — список живых Verified-сессий (онлайн-игроки) для дашборда.
// По nonce админ выбирает игрока для скриншота.
func (h Handler) listOnlineSessions(c fiber.Ctx) error {
	if h.sessions == nil {
		return c.JSON([]yggdrasil.OnlineSession{})
	}
	return c.JSON(h.sessions.ActiveSessions())
}

type requestScreenshotBody struct {
	Nonce string `json:"nonce"`
}

// requestScreenshot — админ запрашивает скриншот экрана онлайн-игрока по nonce.
// Резолвит nonce → (uuid, login) через OnlineSessionsProvider, затем создаёт pending.
func (h Handler) requestScreenshot(c fiber.Ctx) error {
	if h.screenshots == nil || h.sessions == nil {
		return c.Status(http.StatusServiceUnavailable).JSON(ErrorResponse{Message: "Скриншоты выключены"})
	}
	var req requestScreenshotBody
	if err := c.Bind().Body(&req); err != nil || req.Nonce == "" {
		return c.Status(http.StatusBadRequest).JSON(ErrorResponse{Message: "Укажите nonce"})
	}
	sess, ok := h.sessions.SessionByNonce(req.Nonce)
	if !ok {
		return c.Status(http.StatusNotFound).JSON(ErrorResponse{Message: "Игрок не онлайн"})
	}
	admin, _ := auth.CurrentUser(c)
	rec, err := h.screenshots.RequestScreenshot(c.Context(), sess.UUID, sess.Name, req.Nonce, admin.Login)
	if err != nil {
		return c.Status(http.StatusInternalServerError).JSON(ErrorResponse{Message: "Не удалось создать запрос"})
	}
	return c.Status(http.StatusCreated).JSON(rec)
}

func (h Handler) listScreenshots(c fiber.Ctx) error {
	if h.screenshots == nil {
		return c.JSON([]models.Screenshot{})
	}
	limit, _ := strconv.Atoi(c.Query("limit", "100"))
	items, err := h.screenshots.ListScreenshots(c.Context(), limit)
	if err != nil {
		return c.Status(http.StatusInternalServerError).JSON(ErrorResponse{Message: "Не удалось получить скриншоты"})
	}
	return c.JSON(items)
}

func (h Handler) screenshotImage(c fiber.Ctx) error {
	if h.screenshots == nil {
		return c.Status(http.StatusNotFound).JSON(ErrorResponse{Message: "Скриншоты выключены"})
	}
	path, err := h.screenshots.ScreenshotFile(c.Context(), c.Params("id"))
	if err != nil {
		return c.Status(http.StatusNotFound).JSON(ErrorResponse{Message: err.Error()})
	}
	c.Set(fiber.HeaderContentType, "image/jpeg")
	// nosniff: содержимое загружает лаунчер игрока; не даём браузеру дашборда
	// переопределить тип и выполнить не-JPEG как HTML/скрипт.
	c.Set("X-Content-Type-Options", "nosniff")
	return c.SendFile(path)
}

// screenshotClaims accepts only the active session token. The removed
// screenshot-token channel allowed unsigned launcher-provided JPEGs; completion
// now always requires the native pixel HMAC below.
func (h Handler) screenshotClaims(c fiber.Ctx) (LaunchClaims, error) {
	return h.service.VerifySessionToken(launchTokenFromHeader(c))
}

// screenshotPending — клиент опрашивает: есть ли pending-запрос скриншота для
// его игровой сессии (токен → claims.Nonce).
func (h Handler) screenshotPending(c fiber.Ctx) error {
	if h.screenshots == nil {
		return c.Status(http.StatusNoContent).Send(nil)
	}
	claims, err := h.screenshotClaims(c)
	if err != nil {
		return c.Status(http.StatusUnauthorized).JSON(ErrorResponse{Message: "Недействительный токен сессии"})
	}
	rec, ok, err := h.screenshots.PendingScreenshot(c.Context(), claims.Nonce)
	if err != nil {
		return c.Status(http.StatusInternalServerError).JSON(ErrorResponse{Message: "Ошибка запроса скриншота"})
	}
	if !ok {
		return c.Status(http.StatusNoContent).Send(nil)
	}
	return c.JSON(fiber.Map{
		"id":       rec.ID,
		"nonce":    rec.Nonce,
		"login":    rec.Login,
		"userUuid": rec.UserUUID,
	})
}

type screenshotUploadBody struct {
	Width  int    `json:"width"`
	Height int    `json:"height"`
	Data   string `json:"data"` // base64: JPEG от лаунчера, PNG от нативки (подписан)
	// Signature — HMAC-SHA256 пикселей ключом сессии (нативный агент). Обязателен для
	// аплоада из JVM.
	Signature string `json:"signature"`
	// Source — чем снят кадр (dxgi/dxgi-idle/gdi/x11). Диагностика: по картинке
	// «залип захват» и «залипла игра» неразличимы. Подписью НЕ покрыт — мод в JVM может
	// соврать, поэтому только в лог, без последствий для игрока.
	Source string `json:"source"`
}

// screenshotUpload — лаунчер грузит JPEG-скриншот по ID из pending-ответа.
// Аутентификация launch-token (связан с nonce сессии, как и pending). Ранняя
// проверка размера base64 ДО декодирования защищает от memory-DoS (app-wide
// BodyLimit 512МБ позволил бы декодировать ~384МБ в heap).
func (h Handler) screenshotUpload(c fiber.Ctx) error {
	if h.screenshots == nil {
		return c.Status(http.StatusNotFound).JSON(ErrorResponse{Message: "Скриншоты выключены"})
	}
	claims, err := h.screenshotClaims(c)
	if err != nil {
		return c.Status(http.StatusUnauthorized).JSON(ErrorResponse{Message: "Недействительный токен сессии"})
	}
	if !h.screenshotLimiter.allow(claims.UUID) {
		return c.Status(http.StatusTooManyRequests).JSON(ErrorResponse{Message: "Слишком много запросов"})
	}
	var req screenshotUploadBody
	if err := c.Bind().Body(&req); err != nil {
		return c.Status(http.StatusBadRequest).JSON(ErrorResponse{Message: "Некорректный запрос"})
	}
	// Ранняя отсечка oversized base64 до декодирования (защита heap от 512МБ-тела).
	if len(req.Data) > maxBase64Len {
		return c.Status(http.StatusRequestEntityTooLarge).JSON(ErrorResponse{Message: "Скриншот слишком большой"})
	}
	id := c.Params("id")
	// Авторизация ДО декодирования: лаунчер не может грузить чужой скриншот по ID.
	if !h.screenshots.BelongsToNonce(c.Context(), id, claims.Nonce) {
		return c.Status(http.StatusForbidden).JSON(ErrorResponse{Message: "Скриншот не принадлежит сессии"})
	}
	data, err := base64Decode(req.Data)
	if err != nil || len(data) == 0 {
		return c.Status(http.StatusBadRequest).JSON(ErrorResponse{Message: "Некорректные данные скриншота"})
	}
	// Кадр прошёл через JVM, где мог быть подменён модом. Принимаем только то, что
	// подписала нативка: сверяем HMAC пикселей и сами перекодируем PNG → JPEG.
	jpegData, verr := h.service.VerifiedCaptureJPEG(
		claims.Nonce, id, req.Width, req.Height, data, req.Signature)
	if verr != nil {
		slog.Warn("anticheat: скриншот с неверной подписью", "login", claims.Login,
			"uuid", claims.UUID, "id", id, "error", verr)
		_ = h.screenshots.FailScreenshot(c.Context(), id, "подпись кадра не сошлась")
		return c.Status(http.StatusBadRequest).JSON(ErrorResponse{Message: "Подпись кадра не сошлась"})
	}
	data = jpegData
	if err := h.screenshots.CompleteScreenshot(c.Context(), id, data, req.Width, req.Height, captureSource(req.Source)); err != nil {
		return c.Status(http.StatusInternalServerError).JSON(ErrorResponse{Message: "Не удалось сохранить скриншот"})
	}
	slog.Info("anticheat: скриншот принят", "login", claims.Login, "id", id,
		"source", captureSource(req.Source), "width", req.Width, "height", req.Height)
	return c.SendStatus(http.StatusNoContent)
}

type screenshotFailBody struct {
	Reason string `json:"reason"`
}

// screenshotFail — лаунчер сообщает, что не смог захватить экран. Ранний сигнал:
// помечает запись failed сразу, не дожидаясь 60с-таймаута reaper'а. Launch-token +
// BelongsToNonce — только свою сессию.
func (h Handler) screenshotFail(c fiber.Ctx) error {
	if h.screenshots == nil {
		return c.Status(http.StatusNotFound).JSON(ErrorResponse{Message: "Скриншоты выключены"})
	}
	claims, err := h.screenshotClaims(c)
	if err != nil {
		return c.Status(http.StatusUnauthorized).JSON(ErrorResponse{Message: "Недействительный токен сессии"})
	}
	id := c.Params("id")
	if !h.screenshots.BelongsToNonce(c.Context(), id, claims.Nonce) {
		return c.Status(http.StatusForbidden).JSON(ErrorResponse{Message: "Скриншот не принадлежит сессии"})
	}
	var req screenshotFailBody
	_ = c.Bind().Body(&req)
	if err := h.screenshots.FailScreenshot(c.Context(), id, req.Reason); err != nil {
		return c.Status(http.StatusInternalServerError).JSON(ErrorResponse{Message: "Не удалось пометить провал"})
	}
	return c.SendStatus(http.StatusNoContent)
}

// screenshotUploadBodyLimit — per-route лимит тела для аплоада скриншота: сужает
// app-wide 512МБ до потолка base64-JPEG (~12МБ). Отвергает по Content-Length ДО
// того, как Fiber забуферизует тело в память (защита от memory-DoS на ранней стадии).
// Fiber v3 не имеет встроенного bodylimit-middleware — это ручная отсечка.
const ScreenshotMaxBodySize = maxBase64Len + 2*1024*1024 // base64 + JSON-оверhead

// detectMaxBodySize — потолок тела /detect: держатель launch-token иначе мог бы слать
// произвольно большой Details (пишется целиком в Raw) под app-wide 512МБ-лимитом.
const detectMaxBodySize = 64 * 1024

// filesMaxBodySize — потолок тела /files: maxReportedFiles записей по ~(путь + 64 hex).
const filesMaxBodySize = 256 * 1024

// initMaxBodySize — потолок тела /handshake/init (HWID-компоненты + pre-launch детекты).
const initMaxBodySize = 128 * 1024

// requestBodyLimit отвергает слишком большое тело до его разбора хендлером (в Fiber v3
// нет встроенного per-route bodylimit).
//
// Content-Length — только быстрая отсечка: с `Transfer-Encoding: chunked` заголовка нет
// вовсе, и раньше проверка молча пропускала тело до app-wide 512МБ. Поэтому решающая
// проверка — по фактически прочитанному телу. От memory-DoS на этапе буферизации
// защищает app-wide BodyLimit (cmd/server/main.go), здесь — от раздувания БД и CPU.
func requestBodyLimit(maxBytes int, h fiber.Handler) fiber.Handler {
	return func(c fiber.Ctx) error {
		tooLarge := func() error {
			return c.Status(http.StatusRequestEntityTooLarge).
				JSON(ErrorResponse{Message: "Слишком большой запрос"})
		}
		if cl := c.Get("Content-Length"); cl != "" {
			if n, err := strconv.Atoi(cl); err == nil && n > maxBytes {
				return tooLarge()
			}
		}
		if len(c.Body()) > maxBytes {
			return tooLarge()
		}
		return h(c)
	}
}

func screenshotUploadBodyLimit(h fiber.Handler) fiber.Handler {
	return requestBodyLimit(ScreenshotMaxBodySize, h)
}

// launchTokenFromHeader достаёт launch-token только из заголовка (НЕ из query —
// query-токен утекает в логи reverse-proxy). Хелпер устраняет дублирование извлечения
// в нескольких хендлерах; для POST с телом используйте launchTokenFromBody.
func launchTokenFromHeader(c fiber.Ctx) string {
	return c.Get("X-Launch-Token")
}

// launchTokenFromBody достаёт launch-token из тела (приоритет) или заголовка —
// единый путь для POST launch-token-хендлеров (detect/heartbeat/confirm/diag/fail).
func launchTokenFromBody(c fiber.Ctx, bodyToken string) string {
	if bodyToken != "" {
		return bodyToken
	}
	return c.Get("X-Launch-Token")
}

// captureSource нормализует диагностический тег источника кадра перед записью в лог.
// Значение приходит из JVM игрока, то есть из-под контроля потенциального чит-мода:
// в лог нельзя пускать ни перевод строки (подделка соседних записей), ни мегабайт
// мусора. Оставляем только [a-z0-9-] и 16 символов.
func captureSource(s string) string {
	if len(s) > 16 {
		s = s[:16]
	}
	clean := strings.Map(func(r rune) rune {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') || r == '-' {
			return r
		}
		return -1
	}, s)
	if clean == "" {
		return "unknown"
	}
	return clean
}

// base64Decode декодирует base64 (standard или URL-safe) JPEG от лаунчера.
func base64Decode(s string) ([]byte, error) {
	if s == "" {
		return nil, errors.New("empty")
	}
	if b, err := base64.StdEncoding.DecodeString(s); err == nil {
		return b, nil
	}
	return base64.URLEncoding.DecodeString(s)
}
