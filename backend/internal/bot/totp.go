package bot

import (
	"fmt"
	"html"
	"strings"

	"launcher-backend/internal/models"
	"launcher-backend/internal/repo"
	"launcher-backend/internal/telegram"

	"github.com/pquerna/otp/totp"
	"github.com/skip2/go-qrcode"
	"golang.org/x/crypto/bcrypt"
)

func (s *Service) beginTotpFlow(chatID int64, telegramUID int64) error {
	uidPtr, err := s.linkedUID(telegramUID)
	if err != nil {
		return err
	}
	if uidPtr == nil {
		return s.forbidNotLinked(chatID, telegramUID)
	}
	u, err := repo.FindUserByID(s.ctx(), s.DB, *uidPtr)
	if err != nil {
		return err
	}
	if u == nil {
		return fmt.Errorf("нет пользователя")
	}
	if u.TOTPEnabled {
		return s.beginTotpDisable(chatID, telegramUID)
	}
	return s.beginTotpSetup(chatID, telegramUID, u)
}

func (s *Service) beginTotpSetup(chatID int64, telegramUID int64, u *models.User) error {
	issuer := strings.TrimSpace(s.Cfg.TOTPIssuer)
	if issuer == "" {
		issuer = strings.TrimSpace(s.Cfg.BrandPublicName)
	}
	if issuer == "" {
		issuer = "Launcher"
	}
	accLabel := u.Login

	key, err := totp.Generate(totp.GenerateOpts{Issuer: issuer, AccountName: accLabel})
	if err != nil {
		return err
	}
	if err := repo.UpsertTotpSecretPending(s.ctx(), s.DB, u.ID, key.Secret()); err != nil {
		return err
	}
	ep := repo.EmptyPayload()
	if err := repo.SaveDialogue(s.ctx(), s.DB, chatID, repo.FlowTotpConfirm, &ep); err != nil {
		return err
	}

	url := key.URL()
	png, err := qrcode.Encode(url, qrcode.Medium, 256)
	if err != nil {
		return err
	}
	photoCap := "<b>QR для двухфакторной защиты лаунчера</b>\n\n" +
		"1) Откройте приложение-аутентификатор (Google Authenticator, Authy, Microsoft Authenticator и т.д.).\n" +
		"2) Добавьте аккаунт — отсканируйте этот QR камерой <i>внутри приложения</i> или вручную по ключу из следующего сообщения.\n\n" +
		"<i>Не закрывайте чат, пока не сохраните ключ в приложении.</i>"
	if err := telegram.SendPhotoPNG(s.HTTP, s.Cfg.TelegramBotToken, chatID, "totp.png", png, photoCap, nil); err != nil {
		return err
	}

	safeHref := html.EscapeString(url)
	body := fmt.Sprintf(
		"<b>Почти готово</b>\n\n"+
			"Если QR не подошёл — секрет для ручного ввода скрыт под спойлером (нажмите, чтобы раскрыть):\n"+
			"<tg-spoiler><code>%s</code></tg-spoiler>\n\n"+
			"Ссылка <a href=\"%s\">otpauth</a> — на телефоне часто открывает то же приложение.\n\n"+
			"<b>Завершение:</b> когда в приложении появился 6-значный код, введите <b>текущий код</b> (он обновляется каждые ~30 с) <b>одним сообщением</b> сюда.\n\n"+
			"<i>Пока вы не введёте верный код, 2FA для лаунчера не включится.</i>",
		escHTML(key.Secret()), safeHref,
	)
	return s.notifyHTML(chatID, s.msgWithCancelHint(body), homeReplyKeyboardMarkup())
}

func (s *Service) handleTotpConfirm(chatID int64, messageID int, telegramUID int64, text string) error {
	code := strings.ReplaceAll(strings.TrimSpace(text), " ", "")
	uidPtr, err := s.linkedUID(telegramUID)
	if err != nil {
		return err
	}
	if uidPtr == nil {
		_ = repo.ClearDialogue(s.ctx(), s.DB, chatID)
		return s.forbidNotLinked(chatID, telegramUID)
	}
	u, err := repo.FindUserByID(s.ctx(), s.DB, *uidPtr)
	if err != nil {
		return err
	}
	if u == nil || u.TOTPSecret == "" {
		_ = repo.ClearDialogue(s.ctx(), s.DB, chatID)
		return s.notifyWarn(chatID, "Настройку 2FA нужно начать снова: нажмите «2FA».")
	}
	if u.TOTPEnabled {
		_ = repo.ClearDialogue(s.ctx(), s.DB, chatID)
		return s.sendHomeMenu(chatID, telegramUID, "2FA уже включена для этого аккаунта.")
	}
	if len(code) != 6 {
		return s.notifyWarn(chatID, "Нужен код из <b>6 цифр</b> без пробелов (как показывает приложение-аутентификатор прямо сейчас).")
	}
	if !totp.Validate(code, u.TOTPSecret) {
		return s.notifyWarn(chatID, "Код не подошёл. Убедитесь, что время на телефоне включено автоматически, и введите <b>текущий</b> код из приложения.")
	}
	if messageID > 0 {
		s.redactSensitiveMessage(chatID, messageID, "Код 2FA",
			"Под спойлером нет кода — только заглушка. Цифры из приложения в истории не сохраняются.")
	}
	if err := repo.SetTotpEnabled(s.ctx(), s.DB, u.ID, true); err != nil {
		return err
	}
	_ = repo.ClearDialogue(s.ctx(), s.DB, chatID)
	return s.sendHomeMenu(chatID, telegramUID, "✅ <b>2FA включена.</b> При входе в лаунчер потребуется код из приложения.")
}

func (s *Service) beginTotpDisable(chatID int64, telegramUID int64) error {
	uidPtr, err := s.linkedUID(telegramUID)
	if err != nil {
		return err
	}
	if uidPtr == nil {
		return s.forbidNotLinked(chatID, telegramUID)
	}
	u, err := repo.FindUserByID(s.ctx(), s.DB, *uidPtr)
	if err != nil {
		return err
	}
	if u == nil {
		return fmt.Errorf("нет пользователя")
	}
	if !u.TOTPEnabled {
		return s.notifyWarn(chatID, "Двухфакторка сейчас выключена. Включить — кнопка «2FA».")
	}
	ep := repo.EmptyPayload()
	if err := repo.SaveDialogue(s.ctx(), s.DB, chatID, repo.FlowTotpDisablePwd, &ep); err != nil {
		return err
	}
	return s.notifyHTML(chatID, s.msgWithCancelHint(
		"<b>Отключение 2FA</b>\n\n"+
			"Введите <b>пароль от аккаунта</b> (как для входа). Сообщение с паролем бот скроет и удалит из линии чата.\n\n"+
			"Затем попросим код из приложения — это защита от случайного отключения."),
		homeReplyKeyboardMarkup())
}

func (s *Service) handleTotpDisablePwd(chatID int64, messageID int, telegramUID int64, text string) error {
	uidPtr, err := s.linkedUID(telegramUID)
	if err != nil {
		return err
	}
	if uidPtr == nil {
		_ = repo.ClearDialogue(s.ctx(), s.DB, chatID)
		return s.forbidNotLinked(chatID, telegramUID)
	}
	u, err := repo.FindUserByID(s.ctx(), s.DB, *uidPtr)
	if err != nil {
		return err
	}
	if u == nil {
		return fmt.Errorf("нет пользователя")
	}
	if !u.TOTPEnabled {
		_ = repo.ClearDialogue(s.ctx(), s.DB, chatID)
		return s.sendHomeMenu(chatID, telegramUID, "2FA уже выключена.")
	}
	pw := strings.TrimSpace(text)
	if pw != "" && messageID > 0 {
		s.redactPasswordMessage(chatID, messageID, pw)
	}
	if bcrypt.CompareHashAndPassword([]byte(u.PasswordHash), []byte(pw)) != nil {
		_ = s.notifyWarn(chatID, "Пароль не подошёл. Если забыли пароль — смените через «Сменить пароль» или администратора.")
		ep := repo.EmptyPayload()
		return repo.SaveDialogue(s.ctx(), s.DB, chatID, repo.FlowTotpDisablePwd, &ep)
	}
	ep := repo.EmptyPayload()
	if err := repo.SaveDialogue(s.ctx(), s.DB, chatID, repo.FlowTotpDisableOTP, &ep); err != nil {
		return err
	}
	return s.notifyHTML(chatID, s.msgWithCancelHint(
		"Пароль принят. Введите <b>текущий шестизначный код</b> из приложения 2FA — тем самым подтверждаете отключение."),
		homeReplyKeyboardMarkup())
}

func (s *Service) handleTotpDisableOTP(chatID int64, messageID int, telegramUID int64, text string) error {
	code := strings.ReplaceAll(strings.TrimSpace(text), " ", "")
	uidPtr, err := s.linkedUID(telegramUID)
	if err != nil {
		return err
	}
	if uidPtr == nil {
		_ = repo.ClearDialogue(s.ctx(), s.DB, chatID)
		return s.forbidNotLinked(chatID, telegramUID)
	}
	u, err := repo.FindUserByID(s.ctx(), s.DB, *uidPtr)
	if err != nil {
		return err
	}
	if u == nil || !u.TOTPEnabled || u.TOTPSecret == "" {
		_ = repo.ClearDialogue(s.ctx(), s.DB, chatID)
		return s.notifyWarn(chatID, "Неверный код или сессия устарела. Если 2FA уже выключена — откройте «Профиль». Иначе начните снова: «2FA».")
	}
	if len(code) != 6 {
		return s.notifyWarn(chatID, "Нужен код из <b>6 цифр</b> без пробелов (как показывает приложение-аутентификатор прямо сейчас).")
	}
	if !totp.Validate(code, u.TOTPSecret) {
		return s.notifyWarn(chatID, "Код из приложения не совпал. Введите новый код с экрана authenticator (он обновляется каждые ~30 с).")
	}
	if messageID > 0 {
		s.redactSensitiveMessage(chatID, messageID, "Код 2FA",
			"Под спойлером нет кода — только заглушка. Код подтверждения из чата удалён.")
	}
	if err := repo.ClearTotp(s.ctx(), s.DB, u.ID); err != nil {
		return err
	}
	_ = repo.ClearDialogue(s.ctx(), s.DB, chatID)
	return s.sendHomeMenu(chatID, telegramUID, "✅ <b>2FA отключена.</b> В лаунчере снова достаточно логина и пароля.")
}
