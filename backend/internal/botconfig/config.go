// Package botconfig хранит настройки Telegram-бота (отдельный бинарник cmd/bot),
// которые читаются из того же .env, что и backend. Подключение к БД сюда не входит —
// бот использует общий GORM-слой через launcher-backend/internal/database.
package botconfig

import (
	"fmt"
	"os"
	"strings"
)

type Config struct {
	TelegramBotToken string

	BrandPublicName string
	BrandTagline    string
	WelcomeExtra    string

	VPSOpsHealthURL      string
	VPSOpsAlertStatePath string

	ButtonEmojiPrimary     string
	ButtonEmojiSuccess     string
	ButtonEmojiDanger      string
	ButtonEmojiProfile     string
	ButtonEmojiChangePass  string
	ButtonEmojiChangeEmail string
	ButtonEmoji2FA         string
	ButtonEmojiDonate      string
	ButtonEmojiLauncher    string

	AdminAllowlistIDs []int64
	GMLAuthSecret     string
	TOTPIssuer        string
	DonateShopURL     string

	LauncherExePath           string
	LauncherDownloadPublicURL string
	PublicOrigin              string
	BotBannerURL              string
}

// Load читает настройки бота из переменных окружения (.env уже загружен backend-конфигом).
func Load() (*Config, error) {
	token := envTrim("TELEGRAM_BOT_TOKEN")
	if token == "" {
		return nil, fmt.Errorf("TELEGRAM_BOT_TOKEN не задан")
	}
	cfg := &Config{
		TelegramBotToken:       token,
		BrandPublicName:        envOr("BOT_PUBLIC_NAME", "Launcher Accounts"),
		BrandTagline:           envOr("BOT_BRAND_LINE", "Учётные записи игроков."),
		WelcomeExtra:           envOr("BOT_WELCOME_EXTRA", "Привязка аккаунта сайта и управление паролем здесь же."),
		VPSOpsHealthURL:        envTrim("VPS_OPS_HEALTH_URL"),
		VPSOpsAlertStatePath:   envTrim("VPS_OPS_ALERT_STATE_PATH"),
		ButtonEmojiPrimary:     envTrim("BOT_BTN_EMOJI_PRIMARY"),
		ButtonEmojiSuccess:     envTrim("BOT_BTN_EMOJI_SUCCESS"),
		ButtonEmojiDanger:      envTrim("BOT_BTN_EMOJI_DANGER"),
		ButtonEmojiProfile:     envTrim("BOT_BTN_EMOJI_PROFILE"),
		ButtonEmojiChangePass:  envTrim("BOT_BTN_EMOJI_CHANGE_PASSWORD"),
		ButtonEmojiChangeEmail: envTrim("BOT_BTN_EMOJI_CHANGE_EMAIL"),
		ButtonEmoji2FA:         envTrim("BOT_BTN_EMOJI_2FA"),
		ButtonEmojiDonate:      envTrim("BOT_BTN_EMOJI_DONATE"),
		ButtonEmojiLauncher:    envTrim("BOT_BTN_EMOJI_LAUNCHER"),
		AdminAllowlistIDs:      parseIDList(envTrim("ADMIN_TELEGRAM_IDS")),
		GMLAuthSecret:          envTrim("GML_AUTH_SECRET"),
		TOTPIssuer:             envOr("TOTP_ISSUER", envOr("BOT_PUBLIC_NAME", "Launcher")),
		DonateShopURL:          envOr("DONATE_SHOP_URL", "https://shop.likonchik.xyz"),
		LauncherExePath:        envTrim("LAUNCHER_EXE_PATH"),
		LauncherDownloadPublicURL: envTrim("LAUNCHER_DOWNLOAD_URL"),
		PublicOrigin:              strings.TrimRight(envTrim("PUBLIC_BASE_URL"), "/"),
		BotBannerURL:              envTrim("BOT_BANNER_URL"),
	}
	return cfg, nil
}

// LauncherDirectDownloadURL — ссылка на публичную страницу скачивания лаунчера
// (витрина /download на бэкенде: выбор платформы, актуальная версия). Пусто —
// кнопка «Скачать с сайта» не показывается, остаётся отправка файла в чат.
func (c *Config) LauncherDirectDownloadURL() string {
	if ex := strings.TrimSpace(c.LauncherDownloadPublicURL); ex != "" {
		return ex
	}
	if o := strings.TrimSpace(c.PublicOrigin); o != "" {
		return strings.TrimRight(o, "/") + "/download"
	}
	return ""
}

// AdminAllowlisted — пустой список означает «разрешено всем модераторам/админам».
func (c *Config) AdminAllowlisted(telegramUserID int64) bool {
	if len(c.AdminAllowlistIDs) == 0 {
		return true
	}
	for _, id := range c.AdminAllowlistIDs {
		if id == telegramUserID {
			return true
		}
	}
	return false
}

func envTrim(key string) string { return strings.TrimSpace(os.Getenv(key)) }

func envOr(key, def string) string {
	if v := envTrim(key); v != "" {
		return v
	}
	return def
}

func parseIDList(raw string) []int64 {
	var ids []int64
	for _, p := range strings.FieldsFunc(raw, func(r rune) bool { return r == ',' || r == ' ' }) {
		p = strings.TrimSpace(p)
		if p == "" {
			continue
		}
		var id int64
		if _, err := fmt.Sscanf(p, "%d", &id); err == nil {
			ids = append(ids, id)
		}
	}
	return ids
}
