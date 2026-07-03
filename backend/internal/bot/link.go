package bot

import (
	"fmt"
	"strings"

	"launcher-backend/internal/repo"

	"golang.org/x/crypto/bcrypt"
	tele "gopkg.in/telebot.v3"
)

func (s *Service) handleLinkLogin(chatID int64, text string) error {
	login := strings.TrimSpace(text)
	if login == "" || len(login) > 254 {
		return s.notifyWarn(chatID, "Нужна одна строка: ник, логин или почта без пустоты и не длиннее 254 символов.")
	}
	dp := repo.DialoguePayload{}
	loginCopy := login
	dp.Login = &loginCopy
	if err := repo.SaveDialogue(s.ctx(), s.DB, chatID, repo.FlowLinkPassword, &dp); err != nil {
		return err
	}
	return s.notifyHTML(chatID, s.msgWithCancelHint(
		"<b>Шаг 2</b>: отправьте <b>пароль</b> от этой учётной записи одним сообщением.\n\n"+
			"<i>Сообщение с паролем бот удалит из чата и заменит заглушкой со спойлером — так безопаснее.</i>"),
		homeReplyKeyboardMarkup())
}

func (s *Service) handleLinkPassword(chatID int64, messageID int, sender *tele.User, payload repo.DialoguePayload, text string) (repo.DialoguePayload, error) {
	login := payload.Login
	if login == nil || *login == "" {
		ep := repo.EmptyPayload()
		_ = repo.SaveDialogue(s.ctx(), s.DB, chatID, repo.FlowIdle, &ep)
		return ep, fmt.Errorf("нет логина в контексте привязки")
	}

	pw := strings.TrimSpace(text)
	if pw != "" && messageID > 0 {
		s.redactPasswordMessage(chatID, messageID, pw)
	}
	if pw == "" {
		return repo.EmptyPayload(), s.notifyWarn(chatID, "Пароль нужно отправить одним сообщением. Скопируйте целиком, без разбиения на несколько сообщений.")
	}

	user, err := repo.FindUserLogin(s.ctx(), s.DB, *login)
	if err != nil {
		return repo.EmptyPayload(), err
	}
	if user == nil {
		_ = repo.InsertAuthLog(s.ctx(), s.DB, nil, *login, "telegram-bot-link", false, strPtr("not_found"))
		_ = s.notifyWarn(chatID, "Не нашли такую учётку или пароль не подошёл.\nПроверьте раскладку и Caps Lock. Можно снова указать другой ник/логин/почту — я верну вас к первому шагу.")
		ep := repo.EmptyPayload()
		_ = repo.SaveDialogue(s.ctx(), s.DB, chatID, repo.FlowLinkLogin, &ep)
		return ep, nil
	}

	if user.IsBanned || user.IsHwidBanned {
		_ = s.notifyWarn(chatID, "Аккаунт заблокирован. Разблокировка только через администратора.")
		_ = repo.ClearDialogue(s.ctx(), s.DB, chatID)
		return repo.EmptyPayload(), nil
	}

	tgid := telegramUserID(sender)
	if user.TelegramID != nil && *user.TelegramID != tgid {
		_ = s.notifyWarn(chatID, "Эта учётная запись уже привязана к другому Telegram. Войти можно только с того чата или попросите админа сбросить привязку.")
		_ = repo.ClearDialogue(s.ctx(), s.DB, chatID)
		return repo.EmptyPayload(), nil
	}

	if bcrypt.CompareHashAndPassword([]byte(user.PasswordHash), []byte(pw)) != nil {
		ui := user.ID
		_ = repo.InsertAuthLog(s.ctx(), s.DB, &ui, user.Login, "telegram-bot-link", false, strPtr("bad_password"))
		_ = s.notifyWarn(chatID, "Не нашли такую учётку или пароль не подошёл.\nПроверьте раскладку и Caps Lock. Можно снова указать другой ник/логин/почту — я верну вас к первому шагу.")
		ep := repo.EmptyPayload()
		_ = repo.SaveDialogue(s.ctx(), s.DB, chatID, repo.FlowLinkLogin, &ep)
		return ep, nil
	}

	code, err := otpPlain()
	if err != nil {
		return repo.EmptyPayload(), err
	}
	if err := s.persistOTP(user.ID, chatID, purposeLink, code); err != nil {
		return repo.EmptyPayload(), err
	}
	ui := user.ID
	_ = repo.InsertAuthLog(s.ctx(), s.DB, &ui, user.Login, "telegram-bot-link", true, strPtr("password_checked"))

	_ = s.notifyHTML(chatID, s.msgWithCancelHint(fmt.Sprintf(
		"<b>Шаг 3</b>: в чат пришёл одноразовый код (действует <b>%d мин.</b>).\n"+
			"Введите <b>ровно эти 6 цифр</b> отдельным сообщением — так мы убедимся, что это ваш Telegram.\n\n<code>%s</code>",
		otpMinutes, escHTML(code),
	)), homeReplyKeyboardMarkup())

	dp := payload
	dp.OtpUserID = &ui
	dp.Login = nil
	if err := repo.SaveDialogue(s.ctx(), s.DB, chatID, repo.FlowLinkOtp, &dp); err != nil {
		return repo.EmptyPayload(), err
	}
	return dp, nil
}

func (s *Service) handleLinkOTP(chatID int64, sender *tele.User, payload repo.DialoguePayload, code string) error {
	if payload.OtpUserID == nil {
		return fmt.Errorf("нет user id для OTP")
	}
	uid := *payload.OtpUserID
	ok, err := s.consumeOTPCheck(chatID, uid, purposeLink, code)
	if err != nil {
		return err
	}
	if !ok {
		_ = s.notifyWarn(chatID,
			"Код неверный или истёк (коды живут около 10 минут).\nНапишите /cancel и нажмите «Войти», чтобы начать привязку сначала.")
		ep := repo.EmptyPayload()
		_ = repo.SaveDialogue(s.ctx(), s.DB, chatID, repo.FlowLinkLogin, &ep)
		return nil
	}

	uname := sender.Username
	_ = repo.BindTelegram(s.ctx(), s.DB, uid, telegramUserID(sender), ptrStrOrNil(uname))
	_ = repo.InsertAuthLog(s.ctx(), s.DB, &uid, "?", "telegram-bot-link", true, strPtr("linked"))

	_ = repo.ClearDialogue(s.ctx(), s.DB, chatID)
	return s.sendHomeMenu(chatID, telegramUserID(sender), "✅ <b>Аккаунт привязан.</b> Теперь доступны профиль, пароль, почта и 2FA.")
}

func strPtr(s string) *string { return &s }

func ptrStrOrNil(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}
