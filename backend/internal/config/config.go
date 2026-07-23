package config

import (
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"time"
)

type Config struct {
	AppEnv              string
	LogLevel            string
	ServerAddr          string
	AuthMode            string
	AuthProviderURL     string
	DatabaseURL         string
	JWTSecret           string
	SQLitePath          string
	AllowedOrigins      []string
	AdminLogins         []string
	ProfileStorageRoot  string
	// ProfileCDNBase — база публичного зеркала файлов профилей (бакет S3).
	// Задан → манифест отдаёт absolute download_url на бакет, трафик сборок идёт мимо VPS.
	// Пусто → файлы качаются с бэкенда по относительному /api/profiles/... (дефолт).
	// Раскладка ключей в бакете = раскладка storage: <slug>/files/<path>.
	ProfileCDNBase      string
	LauncherReleaseRoot string
	ScreenshotStorageRoot string
	TelegramChannel     string
	PublicBaseURL       string
	YggdrasilKeyPath    string
	YggdrasilServerName string
	AuthlibInjectorPath string
	AnticheatSecret      string
	AnticheatAutoBan     bool
	AnticheatAgentPath   string
	AnticheatNativeLinux  string
	AnticheatNativeWin    string
	AnticheatKickSeverity int
	// AnticheatHeartbeatSeconds — окно живости агента: без heartbeat дольше → сессия гасится.
	AnticheatHeartbeatSeconds int
	// AnticheatRequireAttestation — жёсткая проверка proof в confirm. Включать ТОЛЬКО после
	// раздачи лаунчера с attestation (mandatory-bump), иначе старые клиенты не пройдут confirm.
	AnticheatRequireAttestation bool
	// TokenTTL — срок жизни JWT-сессии (логин в лаунчере и админке).
	TokenTTL time.Duration
	// TrustedProxies — явный список доверенных прокси (IP или CIDR) из TRUSTED_PROXIES.
	// Только с этих адресов X-Forwarded-For принимается как источник реального IP клиента.
	// Пусто — используется дефолт Loopback/Private (см. cmd/server/main.go).
	TrustedProxies []string
	// Алерты античита в Telegram: токен бота (например, vps-ops-bot) и chat_id получателя.
	AnticheatAlertBotToken string
	AnticheatAlertChatID   string
	// P5 — серверно-авторитетный in-game handshake. AnticheatP5Secret — общий секрет
	// для аутентификации игрового NeoForge-сервера (server-to-server, не JWT). Пуст —
	// P5 выключен. AnticheatP5Enforce — false (дефолт): репорт-онли (пускаем, но логируем
	// расхождения); true: кик при невалидном proof (включать ТОЛЬКО после раздачи мода и
	// обкатки на dev-сервере, иначе кикнет всех).
	AnticheatP5Secret  string
	AnticheatP5Enforce bool
}

func Load() Config {
	loadDotEnv(".env")
	loadDotEnv(filepath.Join("backend", ".env"))

	cfg := Config{
		AppEnv:          env("APP_ENV", "development"),
		LogLevel:        strings.ToLower(env("LOG_LEVEL", "info")),
		ServerAddr:      env("SERVER_ADDR", "127.0.0.1:8080"),
		// AUTH_MODE: local — логин валидируется в общей БД (bcrypt+TOTP); http — внешний GML-провайдер.
		AuthMode:        strings.ToLower(env("AUTH_MODE", "local")),
		AuthProviderURL: env("AUTH_PROVIDER_URL", "https://pjm.likonchik.xyz/api/gml/auth"),
		DatabaseURL:     os.Getenv("DATABASE_URL"),
		JWTSecret:       env("JWT_SECRET", "dev-only-change-me"),
		SQLitePath:      env("SQLITE_PATH", filepath.Join("data", "launcher.db")),
		AdminLogins:     splitCSV(env("ADMIN_LOGINS", "")),
		ProfileStorageRoot: env(
			"PROFILE_STORAGE_ROOT",
			filepath.Join("storage", "profiles"),
		),
		ProfileCDNBase: strings.TrimRight(env("PROFILE_CDN_BASE", ""), "/"),
		LauncherReleaseRoot: env(
			"LAUNCHER_RELEASE_ROOT",
			filepath.Join("storage", "releases"),
		),
		ScreenshotStorageRoot: env(
			"SCREENSHOT_STORAGE_ROOT",
			filepath.Join("storage", "screenshots"),
		),
		AllowedOrigins: splitCSV(env(
			"ALLOWED_ORIGINS",
			"http://127.0.0.1:5173,http://localhost:5173,http://127.0.0.1:3000,http://localhost:3000",
		)),
		TelegramChannel:     env("TELEGRAM_NEWS_CHANNEL", "project_minecraft"),
		PublicBaseURL:       strings.TrimRight(env("PUBLIC_BASE_URL", "http://127.0.0.1:8080"), "/"),
		YggdrasilKeyPath:    env("YGGDRASIL_KEY_PATH", filepath.Join("data", "yggdrasil_key.pem")),
		YggdrasilServerName: env("YGGDRASIL_SERVER_NAME", "Project Minecraft"),
		AuthlibInjectorPath: env("AUTHLIB_INJECTOR_PATH", filepath.Join("data", "authlib-injector.jar")),
		AnticheatSecret:     env("ANTICHEAT_SECRET", ""),
		AnticheatAutoBan:    env("ANTICHEAT_AUTO_BAN", "false") == "true",
		AnticheatAgentPath:   env("ANTICHEAT_AGENT_PATH", filepath.Join("data", "anticheat-agent.jar")),
		AnticheatNativeLinux:  env("ANTICHEAT_NATIVE_LINUX", filepath.Join("data", "libanticheat.so")),
		AnticheatNativeWin:    env("ANTICHEAT_NATIVE_WIN", filepath.Join("data", "anticheat.dll")),
		AnticheatKickSeverity: atoiDefault(env("ANTICHEAT_KICK_SEVERITY", "7"), 7),
		AnticheatHeartbeatSeconds: atoiDefault(env("ANTICHEAT_HEARTBEAT_TIMEOUT", "90"), 90),
		AnticheatRequireAttestation: env("ANTICHEAT_REQUIRE_ATTESTATION", "false") == "true",
		TokenTTL:              time.Duration(atoiDefault(env("TOKEN_TTL_HOURS", "168"), 168)) * time.Hour,
		TrustedProxies:        splitCSV(env("TRUSTED_PROXIES", "")),
		AnticheatAlertBotToken: env("ANTICHEAT_ALERT_BOT_TOKEN", ""),
		AnticheatAlertChatID:   env("ANTICHEAT_ALERT_CHAT_ID", ""),
		AnticheatP5Secret:      env("ANTICHEAT_P5_SECRET", ""),
		AnticheatP5Enforce:     env("ANTICHEAT_P5_ENFORCE", "false") == "true",
	}

	if cfg.JWTSecret == "dev-only-change-me" {
		slog.Warn("using development JWT secret")
	}

	// Античит-секрет отдельный от JWT: им подписываются короткоживущие launch-token.
	// Если не задан — деривируем из JWT-секрета, чтобы dev-окружение работало из коробки.
	if cfg.AnticheatSecret == "" {
		cfg.AnticheatSecret = "anticheat:" + cfg.JWTSecret
		slog.Warn("ANTICHEAT_SECRET not set, deriving from JWT secret")
	}

	return cfg
}

// devSecrets — известные дефолты, с которыми нельзя выходить в прод.
var devSecrets = map[string]bool{
	"dev-only-change-me":      true,
	"change-me-in-production": true,
}

// Validate отклоняет конфигурацию, с которой опасно стартовать в production.
func (c Config) Validate() error {
	if c.AppEnv != "production" {
		return nil
	}
	if devSecrets[c.JWTSecret] {
		return errors.New("APP_ENV=production требует настоящий JWT_SECRET (сейчас дев-заглушка)")
	}
	// Античит-секрет (подпись launch-token) в проде должен быть задан ЯВНО и отличаться
	// от JWT: иначе компрометация одного раскрывает второй, а деривация предсказуема.
	if c.AnticheatSecret == "anticheat:"+c.JWTSecret {
		return errors.New("APP_ENV=production требует явный ANTICHEAT_SECRET (сейчас деривируется из JWT_SECRET)")
	}
	if c.AnticheatSecret == c.JWTSecret {
		return errors.New("ANTICHEAT_SECRET должен отличаться от JWT_SECRET")
	}
	if devSecrets[c.AnticheatSecret] {
		return errors.New("APP_ENV=production требует настоящий ANTICHEAT_SECRET (сейчас дев-заглушка)")
	}
	// В production обязателен Postgres (DATABASE_URL). Пустой URL молча уводит на SQLite,
	// что в проде даёт split-brain аккаунтов/банов между репликами и потерю данных.
	if c.DatabaseURL == "" {
		return errors.New("APP_ENV=production требует DATABASE_URL (Postgres): тихий SQLite-fallback запрещён")
	}
	return nil
}

// SlogLevel переводит LOG_LEVEL в slog.Level (debug/info/warn/error).
func (c Config) SlogLevel() slog.Level {
	switch c.LogLevel {
	case "debug":
		return slog.LevelDebug
	case "warn":
		return slog.LevelWarn
	case "error":
		return slog.LevelError
	default:
		return slog.LevelInfo
	}
}

func atoiDefault(value string, fallback int) int {
	n := 0
	for _, r := range value {
		if r < '0' || r > '9' {
			return fallback
		}
		n = n*10 + int(r-'0')
	}
	if value == "" {
		return fallback
	}
	return n
}

func splitCSV(value string) []string {
	parts := strings.Split(value, ",")
	items := make([]string, 0, len(parts))
	for _, part := range parts {
		item := strings.TrimSpace(part)
		if item != "" {
			items = append(items, item)
		}
	}
	return items
}

func env(key, fallback string) string {
	value := os.Getenv(key)
	if value == "" {
		return fallback
	}
	return value
}

func loadDotEnv(path string) {
	if _, err := os.Stat(path); err != nil {
		return
	}

	data, err := os.ReadFile(path)
	if err != nil {
		slog.Warn("failed to read env file", "path", path, "error", err)
		return
	}

	for _, line := range parseEnvLines(string(data)) {
		if os.Getenv(line.key) == "" {
			_ = os.Setenv(line.key, line.value)
		}
	}
}

type envLine struct {
	key   string
	value string
}

func parseEnvLines(input string) []envLine {
	lines := make([]envLine, 0)
	for _, rawLine := range splitLines(input) {
		line := trim(rawLine)
		if line == "" || line[0] == '#' {
			continue
		}

		key, value, ok := cut(line, "=")
		if !ok || key == "" {
			continue
		}
		lines = append(lines, envLine{key: trim(key), value: trimQuotes(trim(value))})
	}
	return lines
}

func splitLines(input string) []string {
	lines := make([]string, 0)
	start := 0
	for i, ch := range input {
		if ch == '\n' {
			lines = append(lines, input[start:i])
			start = i + 1
		}
	}
	if start <= len(input) {
		lines = append(lines, input[start:])
	}
	return lines
}

func trim(value string) string {
	start := 0
	for start < len(value) && (value[start] == ' ' || value[start] == '\t' || value[start] == '\r') {
		start++
	}
	end := len(value)
	for end > start && (value[end-1] == ' ' || value[end-1] == '\t' || value[end-1] == '\r') {
		end--
	}
	return value[start:end]
}

func trimQuotes(value string) string {
	if len(value) >= 2 {
		if (value[0] == '"' && value[len(value)-1] == '"') || (value[0] == '\'' && value[len(value)-1] == '\'') {
			return value[1 : len(value)-1]
		}
	}
	return value
}

func cut(value, sep string) (string, string, bool) {
	for i := 0; i <= len(value)-len(sep); i++ {
		if value[i:i+len(sep)] == sep {
			return value[:i], value[i+len(sep):], true
		}
	}
	return value, "", false
}
