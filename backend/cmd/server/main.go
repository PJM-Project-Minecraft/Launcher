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
	"launcher-backend/internal/events"
	"launcher-backend/internal/launcherrelease"
	"launcher-backend/internal/middleware"
	"launcher-backend/internal/news"
	"launcher-backend/internal/profiles"
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

	app := fiber.New(fiber.Config{
		AppName: "Launcher Backend",
		// Бэкенд стоит за nginx: настоящий IP клиента приходит в X-Forwarded-For.
		ProxyHeader:      fiber.HeaderXForwardedFor,
		TrustProxy:       true,
		TrustProxyConfig: fiber.TrustProxyConfig{Loopback: true, LinkLocal: true, Private: true},
		// Лимит тела запроса: загрузка бинарников релизов лаунчера через админку.
		BodyLimit: 512 * 1024 * 1024,
	})
	app.Use(middleware.CORS(cfg.AllowedOrigins))

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
	adminapi.NewHandler(db).RegisterRoutes(app, authService.RequireAuth())
	profilesBroker := events.NewBroker()
	profiles.NewHandler(profiles.NewService(db, cfg.ProfileStorageRoot), profilesBroker).
		RegisterRoutes(app, authService.RequireAuth())
	releaseService := launcherrelease.NewService(db, cfg.LauncherReleaseRoot)
	launcherrelease.NewHandler(releaseService, profilesBroker).
		RegisterRoutes(app, authService.RequireAuth())
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
	acService.StartHeartbeatReaper(30 * time.Second)
	if notifier := anticheat.NewTelegramNotifier(cfg.AnticheatAlertBotToken, cfg.AnticheatAlertChatID); notifier != nil {
		acService.SetNotifier(notifier)
		slog.Info("anticheat: telegram alerts enabled", "chat_id", cfg.AnticheatAlertChatID)
	}
	anticheat.NewHandler(acService).
		WithVersionGate(releaseService).
		RegisterRoutes(app, authService.RequireAuth())

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
