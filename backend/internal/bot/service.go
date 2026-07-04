// Package bot — Telegram-бот учётных записей (регистрация, привязка, пароль, 2FA, админка),
// перенесён из отдельного проекта и работает поверх общего GORM-слоя (launcher-backend).
package bot

import (
	"context"
	"crypto/rand"
	"fmt"
	"log/slog"
	"math/big"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"launcher-backend/internal/botconfig"
	"launcher-backend/internal/launcherrelease"
	"launcher-backend/internal/models"
	"launcher-backend/internal/repo"
	"launcher-backend/internal/telegram"

	"golang.org/x/crypto/bcrypt"
	tele "gopkg.in/telebot.v3"
	"gorm.io/gorm"
)

const (
	purposeLink  = "telegram_link"
	purposePwd   = "password_change"
	purposeEmail = "email_change"
	otpMinutes   = 10
	lettersPw    = "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789"

	labelBtnProfile        = "Профиль"
	labelBtnChangePassword = "Сменить пароль"
	labelBtnEmail          = "Email"
	labelBtn2FA            = "2FA"
	donateKeyboardLabel    = "Донат"
	launcherKeyboardLabel  = "Скачать лаунчер"
)

type Service struct {
	DB   *gorm.DB
	Cfg  *botconfig.Config
	HTTP *http.Client
	Log  *slog.Logger

	// Releases — сервис релизов лаунчера (общая БД с cmd/server). Нулевой
	// (storageRoot пустой) — кнопки скачивания по платформам скрыты.
	Releases launcherrelease.Service
}

type adminContext struct {
	user models.User
}

func telegramUserID(u *tele.User) int64 {
	if u == nil {
		return 0
	}
	return u.ID
}

func (s *Service) ctx() context.Context { return context.Background() }

func escHTML(t string) string {
	return strings.NewReplacer("&", "&amp;", "<", "&lt;", ">", "&gt;").Replace(t)
}

func escHTMLAttr(t string) string {
	return strings.NewReplacer("&", "&amp;", `"`, "&quot;").Replace(t)
}

func otpPlain() (string, error) {
	n, err := rand.Int(rand.Reader, big.NewInt(900000))
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("%06d", 100000+n.Int64()), nil
}

func randPassword14() (string, error) {
	b := make([]byte, 14)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	for i := range b {
		b[i] = lettersPw[int(b[i])%len(lettersPw)]
	}
	return string(b), nil
}

func (s *Service) notifyHTML(chatID int64, html string, markup map[string]any) error {
	return telegram.SendHTTPMessageHTML(s.HTTP, s.Cfg.TelegramBotToken, chatID, html, markup, true)
}

func (s *Service) notifyWarn(chatID int64, text string) error {
	return s.notifyHTML(chatID, "⚠ "+escHTML(text), nil)
}

func (s *Service) msgWithCancelHint(html string) string {
	return html + "\n\n<i>Отмена: /cancel</i>"
}

func (s *Service) redactSensitiveMessage(chatID int64, messageID int, title string, footnote string) {
	if messageID <= 0 {
		return
	}
	if err := telegram.DeleteMessage(s.HTTP, s.Cfg.TelegramBotToken, chatID, messageID); err != nil && s.Log != nil {
		s.Log.Warn("удаление конфиденциального сообщения", "err", err)
	}
	const spoilerHidden = "​"
	html := fmt.Sprintf(
		"🔐 <b>%s</b>\nСообщение <b>удалено</b> из чата.\n\n<tg-spoiler>%s</tg-spoiler>\n<i>%s</i>",
		escHTML(title), spoilerHidden, footnote,
	)
	if err := s.notifyHTML(chatID, html, nil); err != nil && s.Log != nil {
		s.Log.Warn("уведомление после скрытия сообщения", "err", err)
	}
}

func (s *Service) redactPasswordMessage(chatID int64, messageID int, rawPassword string) {
	rawPassword = strings.TrimSpace(rawPassword)
	if rawPassword == "" || messageID <= 0 {
		return
	}
	s.redactSensitiveMessage(chatID, messageID, "Ввод пароля",
		"Под спойлером нет пароля — только скрытая заглушка. Реальный пароль в чат не отправляется.")
}

func (s *Service) persistOTP(userID string, chatID int64, purpose, plain string) error {
	hash, err := bcrypt.GenerateFromPassword([]byte(plain), 10)
	if err != nil {
		return err
	}
	exp := time.Now().UTC().Add(time.Duration(otpMinutes) * time.Minute)
	return repo.InsertOTP(s.ctx(), s.DB, userID, chatID, purpose, string(hash), exp)
}

func (s *Service) consumeOTPCheck(chatID int64, userID, purpose, code string) (bool, error) {
	if len(strings.TrimSpace(code)) != 6 {
		return false, nil
	}
	id, hash, ok, err := repo.FindValidOTP(s.ctx(), s.DB, chatID, userID, purpose)
	if err != nil || !ok {
		return false, err
	}
	if err := bcrypt.CompareHashAndPassword([]byte(hash), []byte(strings.TrimSpace(code))); err != nil {
		return false, nil
	}
	if err := repo.ConsumeOTP(s.ctx(), s.DB, id); err != nil {
		return false, err
	}
	return true, nil
}

func (s *Service) linkedUID(telegramUID int64) (*string, error) {
	u, err := repo.FindUserByTelegram(s.ctx(), s.DB, telegramUID)
	if err != nil {
		return nil, err
	}
	if u == nil {
		return nil, nil
	}
	id := u.ID
	return &id, nil
}

func (s *Service) forbidNotLinked(chatID int64, telegramUID int64) error {
	return s.sendHomeMenu(chatID, telegramUID,
		"⚠ Раздел доступен после привязки аккаунта — «Войти» или «Регистрация».")
}

func (s *Service) resolveAdmin(telegramUID int64) (*adminContext, error) {
	me, err := repo.FindUserByTelegram(s.ctx(), s.DB, telegramUID)
	if err != nil {
		return nil, err
	}
	if me == nil {
		return nil, nil
	}
	if !me.IsPrivileged() || !s.Cfg.AdminAllowlisted(telegramUID) {
		return nil, nil
	}
	return &adminContext{user: *me}, nil
}

func maskEmailUnsafe(mail string) string {
	u, d, ok := strings.Cut(mail, "@")
	if !ok {
		return "•••"
	}
	head := ""
	for i, r := range u {
		if i >= 2 {
			break
		}
		head += string(r)
	}
	return head + "…@" + d
}

func (s *Service) launcherExePath() string { return strings.TrimSpace(s.Cfg.LauncherExePath) }

// launcherReleaseInfo — найденный последний релиз под платформу для меню.
type launcherReleaseInfo struct {
	Version  string
	FileName string
	AbsPath  string
	Size     int64
}

// latestLauncherInfo возвращает данные последнего активного релиза под платформу
// или nil, если сервис не сконфигурирован / релизов нет. Ошибки логируются на warn.
func (s *Service) latestLauncherInfo(platform string) *launcherReleaseInfo {
	if s.Releases.StorageRoot() == "" {
		return nil
	}
	release, file, abs, err := s.Releases.LatestFile(s.ctx(), platform)
	if err != nil {
		if s.Log != nil {
			s.Log.Warn("launcher latest release", "platform", platform, "err", err)
		}
		return nil
	}
	return &launcherReleaseInfo{
		Version:  release.Version,
		FileName: file.FileName,
		AbsPath:  abs,
		Size:     file.Size,
	}
}

func (s *Service) replyLauncherDownload(chatID int64, telegramUID int64) error {
	kb := homeReplyKeyboardMarkup()
	raw := s.launcherExePath()
	if raw == "" {
		return s.notifyWarn(chatID, "Скачивание лаунчера временно недоступно. Напишите команде проекта.")
	}
	path := filepath.Clean(raw)
	if _, err := os.Stat(path); err != nil {
		if s.Log != nil {
			s.Log.Warn("launcher exe", "path", path, "err", err)
		}
		return s.notifyWarn(chatID, "Файл лаунчера сейчас недоступен. Попробуйте позже.")
	}

	sendPath, attachName, cleanup, gz, err := prepareLauncherForTelegram(path)
	if err != nil {
		if s.Log != nil {
			s.Log.Error("launcher prepare", "err", err)
		}
		return s.notifyWarn(chatID, "Не удалось подготовить файл лаунчера. Попробуйте позже.")
	}
	defer cleanup()

	caption := "<b>Лаунчер</b>\n\n<i>Сохраните файл и запустите. Вход — учётка сайта (привязка в этом боте).</i>"
	if gz {
		caption = "<b>Лаунчер</b> (архив <code>gzip</code>)\n\n" +
			"Исходный .exe больше лимита Telegram (~50 МБ), поэтому прислан сжатый файл <b>" + escHTML(attachName) + "</b>.\n" +
			"Распакуйте (7-Zip, WinRAR, PeaZip или <code>gunzip</code> в Linux) — получится обычный .exe.\n\n" +
			"<i>После распаковки запустите клиент. Вход — учётка сайта (привязка в этом боте).</i>"
	}
	if dl := s.Cfg.LauncherDirectDownloadURL(); dl != "" {
		caption += "\n\n<b>Прямая ссылка с сервера:</b> <a href=\"" + escHTMLAttr(dl) + "\">скачать .exe</a>"
	}

	docHTTP := s.HTTP
	if docHTTP == nil {
		docHTTP = http.DefaultClient
	}
	longClient := &http.Client{Transport: docHTTP.Transport, Timeout: 20 * time.Minute}
	if longClient.Transport == nil {
		longClient.Transport = http.DefaultTransport
	}
	if err := telegram.SendDocument(longClient, s.Cfg.TelegramBotToken, chatID, sendPath, attachName, caption, kb); err != nil {
		if s.Log != nil {
			s.Log.Error("sendDocument launcher", "err", err)
		}
		return s.notifyWarn(chatID, "Не удалось отправить файл. Попробуйте позже или скачайте по прямой ссылке из раздела «Лаунчер».")
	}
	return nil
}

// replyLauncherReleaseDownload отправляет в чат бинарник последнего активного
// релиза под указанную платформу (linux-x64 / windows-x64). Используется
// кнопками «🐧 Linux» / «🪟 Windows» в разделе «Лаунчер».
func (s *Service) replyLauncherReleaseDownload(chatID int64, platform string) error {
	kb := homeReplyKeyboardMarkup()
	info := s.latestLauncherInfo(platform)
	if info == nil {
		return s.notifyWarn(chatID, "Файл лаунчера сейчас недоступен. Попробуйте позже.")
	}
	if _, err := os.Stat(info.AbsPath); err != nil {
		if s.Log != nil {
			s.Log.Warn("launcher release file", "platform", platform, "path", info.AbsPath, "err", err)
		}
		return s.notifyWarn(chatID, "Файл лаунчера сейчас недоступен. Попробуйте позже.")
	}

	sendPath, attachName, cleanup, gz, err := prepareLauncherForTelegram(info.AbsPath)
	if err != nil {
		if s.Log != nil {
			s.Log.Error("launcher prepare", "platform", platform, "err", err)
		}
		return s.notifyWarn(chatID, "Не удалось подготовить файл лаунчера. Попробуйте позже.")
	}
	defer cleanup()

	platLabel := "Linux"
	if platform == "windows-x64" {
		platLabel = "Windows"
	}
	caption := fmt.Sprintf(
		"<b>Лаунчер</b> v%s (%s)\n\n<i>Сохраните файл и запустите. Вход — учётка сайта (привязка в этом боте).</i>",
		escHTML(info.Version), platLabel,
	)
	if gz {
		caption = fmt.Sprintf(
			"<b>Лаунчер</b> v%s (%s, архив <code>gzip</code>)\n\n"+
				"Исходный файл больше лимита Telegram (~50 МБ), поэтому прислан сжатый <b>%s</b>.\n"+
				"Распакуйте (7-Zip, WinRAR, PeaZip или <code>gunzip</code> в Linux) — получится исполняемый файл.\n\n"+
				"<i>После распаковки запустите клиент. Вход — учётка сайта (привязка в этом боте).</i>",
			escHTML(info.Version), platLabel, escHTML(attachName),
		)
	}
	if dl := s.Cfg.LauncherDirectDownloadURL(); dl != "" {
		caption += "\n\n<b>Прямая ссылка с сервера:</b> <a href=\"" + escHTMLAttr(dl) + "\">скачать</a>"
	}

	docHTTP := s.HTTP
	if docHTTP == nil {
		docHTTP = http.DefaultClient
	}
	longClient := &http.Client{Transport: docHTTP.Transport, Timeout: 20 * time.Minute}
	if longClient.Transport == nil {
		longClient.Transport = http.DefaultTransport
	}
	if err := telegram.SendDocument(longClient, s.Cfg.TelegramBotToken, chatID, sendPath, attachName, caption, kb); err != nil {
		if s.Log != nil {
			s.Log.Error("sendDocument launcher release", "platform", platform, "err", err)
		}
		return s.notifyWarn(chatID, "Не удалось отправить файл. Попробуйте позже или скачайте по прямой ссылке из раздела «Лаунчер».")
	}
	return nil
}

func (s *Service) replyDonateShop(chatID int64, telegramUID int64) error {
	kb := homeReplyKeyboardMarkup()
	url := strings.TrimSpace(s.Cfg.DonateShopURL)
	if url == "" {
		url = "https://shop.likonchik.xyz"
	}
	if !strings.HasPrefix(url, "http://") && !strings.HasPrefix(url, "https://") {
		url = "https://" + url
	}
	display := strings.TrimPrefix(strings.TrimPrefix(url, "https://"), "http://")
	display = strings.TrimSuffix(display, "/")
	html := fmt.Sprintf(
		"<b>Донат и магазин</b>\n\nОткройте витрину по ссылке:\n<a href=\"%s\">%s</a>\n\n"+
			"<i>Покупки и оплата проходят на сайте магазина.</i>",
		escHTML(url), escHTML(display))
	return s.notifyHTML(chatID, html, kb)
}
