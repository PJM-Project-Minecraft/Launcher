package main

import (
	"log/slog"
	"os"
	"time"

	"launcher-backend/internal/adminapi"
	"launcher-backend/internal/anticheat"
	"launcher-backend/internal/auth"
	"launcher-backend/internal/config"
	"launcher-backend/internal/database"
	"launcher-backend/internal/delivery"
	"launcher-backend/internal/events"
	"launcher-backend/internal/gameapi"
	"launcher-backend/internal/launcherrelease"
	"launcher-backend/internal/middleware"
	"launcher-backend/internal/news"
	"launcher-backend/internal/policy"
	"launcher-backend/internal/profiles"
	"launcher-backend/internal/purchases"
	"launcher-backend/internal/shop"
	"launcher-backend/internal/yggdrasil"

	"github.com/gofiber/fiber/v3"
	"github.com/gofiber/fiber/v3/middleware/limiter"
)

func main() {
	cfg := config.Load()
	slog.SetDefault(slog.New(slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{
		Level: cfg.SlogLevel(),
	})))
	if err := cfg.Validate(); err != nil {
		slog.Error("invalid configuration", "error", err)
		os.Exit(1)
	}

	db, err := database.Open(cfg)
	if err != nil {
		slog.Error("database connection failed", "error", err)
		os.Exit(1)
	}
	if err := database.AutoMigrate(db); err != nil {
		slog.Error("database migration failed", "error", err)
		os.Exit(1)
	}
	shopService := shop.NewService(db, cfg.ShopStorageRoot)

	// Доверие X-Forwarded-For: c.IP() (в т.ч. ключ брутфорс-лимитера) берёт первый
	// ВАЛИДНЫЙ адрес из XFF ТОЛЬКО если непосредственный peer — доверенный прокси.
	//
	// ПРЕДУПРЕЖДЕНИЕ О СПУФИНГЕ: если апстрим-прокси лишь ДОБАВЛЯЕТ X-Forwarded-For, не
	// затирая пришедший от клиента, злоумышленник может подделать свой IP и обойти лимитер.
	// Поэтому в проде задавайте TRUSTED_PROXIES точным IP/подсетью апстрима (Reverse Proxy
	// хостинга), а сам прокси должен перезаписывать заголовок. Пустой TRUSTED_PROXIES
	// оставляет исторический дефолт (Loopback/Private) — безопасен, только когда апстрим
	// реально находится в одной из этих подсетей.
	trustCfg := fiber.TrustProxyConfig{Loopback: true, LinkLocal: true, Private: true}
	if len(cfg.TrustedProxies) > 0 {
		trustCfg.Proxies = cfg.TrustedProxies
		slog.Info("trusted proxies configured", "proxies", cfg.TrustedProxies)
	}
	app := fiber.New(fiber.Config{
		AppName: "Launcher Backend",
		// Бэкенд стоит за reverse-прокси: настоящий IP клиента приходит в X-Forwarded-For.
		ProxyHeader: fiber.HeaderXForwardedFor,
		TrustProxy:  true,
		// Без валидации Fiber отдаёт из c.IP() СЫРУЮ строку заголовка целиком. Клиент
		// дописывал в неё что угодно и получал новый ключ брутфорс-лимитера на каждую
		// попытку (а также произвольную строку в PolicyConsent.IP и IP игровой сессии).
		EnableIPValidation: true,
		TrustProxyConfig:   trustCfg,
		// Лимит тела запроса: загрузка бинарников релизов лаунчера через админку.
		BodyLimit: 512 * 1024 * 1024,
	})
	app.Use(middleware.CORS(cfg.AllowedOrigins))
	app.Use(middleware.ManifestCompression())

	// Брутфорс-защита: лимит по IP на эндпоинты, принимающие пароль.
	authLimiter := limiter.New(limiter.Config{
		Max:        10,
		Expiration: time.Minute,
	})
	app.Use("/api/auth/login", authLimiter)
	app.Use("/api/gml/auth", authLimiter)
	app.Use("/api/yggdrasil/authserver/authenticate", authLimiter)

	app.Get("/health", func(c fiber.Ctx) error {
		return c.JSON(fiber.Map{
			"ok":       true,
			"provider": cfg.AuthProviderURL,
		})
	})

	// Баннер Telegram-бота (link-preview шапка меню). Файл кладётся в data/ руками
	// (scp на прод, как агенты античита); нет файла — 404, бот работает без баннера.
	app.Get("/api/public/bot-banner.png", func(c fiber.Ctx) error {
		const p = "data/bot-banner.png"
		if _, err := os.Stat(p); err != nil {
			return c.SendStatus(fiber.StatusNotFound)
		}
		return c.SendFile(p)
	})

	// Выбор источника аутентификации: local — проверка в общей БД (bcrypt+TOTP),
	// http — внешний GML-провайдер (обратная совместимость).
	var provider auth.Provider
	if cfg.AuthMode == "http" {
		provider = auth.NewHTTPProvider(cfg.AuthProviderURL)
		slog.Info("auth provider", "mode", "http", "url", cfg.AuthProviderURL)
	} else {
		provider = auth.NewLocalProvider(db)
		slog.Info("auth provider", "mode", "local")
	}

	authService := auth.NewService(db, provider, cfg.JWTSecret, cfg.AdminLogins, cfg.AppEnv, cfg.TokenTTL)
	auth.NewHandler(authService).RegisterRoutes(app)
	policy.NewHandler(db).RegisterRoutes(app, authService.RequireAuth(), auth.CurrentUser)
	adminapi.NewHandler(db).RegisterRoutes(app, authService.RequireAuth())
	purchases.NewHandler(db, cfg.SiteOrderSecret).RegisterRoutes(app, authService.RequireAuth())
	shop.NewHandler(shopService).RegisterRoutes(app, authService.RequireAuth())
	profilesBroker := events.NewBroker()
	deliveryService, err := delivery.NewService(db, cfg.DeliveryRoot, cfg.ProfileStorageRoot, cfg.LauncherReleaseRoot, cfg.DeliveryManifestSigningKey)
	if err != nil {
		slog.Error("delivery v2 initialization failed", "error", err)
		os.Exit(1)
	}
	delivery.NewHandler(deliveryService).RegisterRoutes(app, authService.RequireAuth())
	releaseService := launcherrelease.NewService(db, cfg.LauncherReleaseRoot).RequireSignatures()
	delivery.NewWatcher(deliveryService, profilesBroker, releaseService.InvalidateDeliveryChannel).Start()
	v1BridgeUntil := time.Time{}
	if cfg.DeliveryV1Bridge {
		v1BridgeUntil = cfg.DeliveryV1BridgeUntil
	}
	profiles.NewHandler(profiles.NewService(db, cfg.ProfileStorageRoot, cfg.ProfileCDNBase), profilesBroker).
		RegisterRoutesWithV1BridgeUntil(app, authService.RequireAuth(), v1BridgeUntil)
	launcherrelease.SetLogger(slog.Warn)
	launcherrelease.NewHandler(releaseService, profilesBroker, deliveryService).
		RegisterRoutesWithV1BridgeUntil(app, authService.RequireAuth(), v1BridgeUntil)
	news.NewHandler(news.NewService(cfg.TelegramChannel)).
		RegisterRoutes(app, authService.RequireAuth())

	yggKeys, err := yggdrasil.LoadOrCreateKey(cfg.YggdrasilKeyPath)
	if err != nil {
		slog.Error("yggdrasil key init failed", "error", err)
		os.Exit(1)
	}
	yggService := yggdrasil.NewService(db, yggKeys, cfg.PublicBaseURL, cfg.YggdrasilServerName, cfg.AuthlibInjectorPath)
	yggdrasil.NewHandler(yggService).RegisterRoutes(app, authService.RequireAuth())

	// Античит связан с yggdrasil-store: confirm помечает игровую сессию Verified.
	acService := anticheat.NewService(db, cfg.AnticheatSecret, cfg.AnticheatAutoBan, yggService.Store(), cfg.AnticheatAgentPath)
	acService.SetNativePaths(cfg.AnticheatNativeLinux, cfg.AnticheatNativeWin)
	acService.SetAuthlibPath(cfg.AuthlibInjectorPath)
	acService.SetKickSeverity(cfg.AnticheatKickSeverity)
	acService.SetHeartbeatTimeout(time.Duration(cfg.AnticheatHeartbeatSeconds) * time.Second)
	acService.SetRequireAttestation(cfg.AnticheatRequireAttestation)
	acService.SetEnforceUnknownMods(cfg.AnticheatUnknownModEnforce)
	acService.StartHeartbeatReaper(30 * time.Second)
	if notifier := anticheat.NewTelegramNotifier(cfg.AnticheatAlertBotToken, cfg.AnticheatAlertChatID); notifier != nil {
		acService.SetNotifier(notifier)
		slog.Info("anticheat: telegram alerts enabled", "chat_id", cfg.AnticheatAlertChatID)
	}
	screenshotSvc := anticheat.NewScreenshotService(db, cfg.ScreenshotStorageRoot)
	screenshotSvc.SetDetector(acService) // серия проваленных скриншотов → детект
	screenshotSvc.StartReaper(30 * time.Second)
	anticheat.NewHandler(acService).
		WithVersionGate(releaseService).
		WithScreenshots(screenshotSvc, yggService.Store()).
		WithP5(cfg.AnticheatP5Secret, cfg.AnticheatP5Enforce).
		RegisterRoutes(app, authService.RequireAuth())
	if cfg.AnticheatP5Secret != "" {
		slog.Info("anticheat: P5 in-game handshake enabled", "enforce", cfg.AnticheatP5Enforce)
	}

	// Игровые эндпоинты мода PJM BaseMod (профиль игрока, очередь правок XP).
	gameapi.NewHandler(db, cfg.GameAPISecret, purchases.NewService(db)).RegisterRoutes(app)

	slog.Info(
		"backend listening",
		"addr", cfg.ServerAddr,
		"auth_provider", cfg.AuthProviderURL,
		"profile_storage_root", cfg.ProfileStorageRoot,
	)
	if err := app.Listen(cfg.ServerAddr); err != nil {
		slog.Error("backend stopped", "error", err)
		os.Exit(1)
	}
}
