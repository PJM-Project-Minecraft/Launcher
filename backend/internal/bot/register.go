package bot

import (
	"errors"
	"fmt"
	"regexp"
	"strings"

	"launcher-backend/internal/repo"

	"golang.org/x/crypto/bcrypt"
	tele "gopkg.in/telebot.v3"
)

var regUsernameRe = regexp.MustCompile(`^[a-zA-Z0-9_]{3,32}$`)

func (s *Service) alreadyLinkedChat(chatID int64, sender *tele.User) (bool, error) {
	tgid := telegramUserID(sender)
	u, err := repo.FindUserByTelegram(s.ctx(), s.DB, tgid)
	if err != nil {
		return false, err
	}
	if u != nil {
		_ = repo.ClearDialogue(s.ctx(), s.DB, chatID)
		_ = s.sendHomeMenu(chatID, tgid, "У вас уже привязан аккаунт к этому Telegram — вход и регистрация не нужны.")
		return true, nil
	}
	return false, nil
}

func (s *Service) beginLoginFlow(chatID int64, sender *tele.User) error {
	if ok, err := s.alreadyLinkedChat(chatID, sender); err != nil || ok {
		return err
	}
	dp := repo.EmptyPayload()
	dp.Login = nil
	_ = repo.SaveDialogue(s.ctx(), s.DB, chatID, repo.FlowLinkLogin, &dp)
	return s.notifyHTML(chatID,
		s.msgWithCancelHint(
			"<b>Шаг 1 из 3</b> (привязка)\n\n"+
				"Введите, кого привязываем:\n"+
				"• <b>игровой ник</b> — если он уже есть в базе;\n"+
				"• иначе <b>логин</b>, которым вы входили на сайте, или <b>e-mail</b> учётки.\n\n"+
				"<i>Один ответ — одно сообщение, без лишнего текста.</i>"),
		homeReplyKeyboardMarkup())
}

func (s *Service) beginRegisterFlow(chatID int64, sender *tele.User) error {
	if ok, err := s.alreadyLinkedChat(chatID, sender); err != nil || ok {
		return err
	}
	ep := repo.EmptyPayload()
	if err := repo.SaveDialogue(s.ctx(), s.DB, chatID, repo.FlowRegUsername, &ep); err != nil {
		return err
	}
	return s.notifyHTML(chatID, s.msgWithCancelHint(
		"<b>Регистрация, шаг 1</b>\n\n"+
			"Придумайте <b>логин</b> (в игре и на сайте будет тот же ник, пока админ не сменит): латиница, цифры или символ <code>_</code>, от 3 до 32 символов.\n\n"+
			"<i>Пример: <code>my_player_01</code></i>"),
		homeReplyKeyboardMarkup())
}

func (s *Service) handleRegUsername(chatID int64, sender *tele.User, text string) error {
	if ok, err := s.alreadyLinkedChat(chatID, sender); err != nil || ok {
		return err
	}
	u := strings.TrimSpace(text)
	if !regUsernameRe.MatchString(u) {
		return s.notifyWarn(chatID, "Логин: только латиница (A–Z, a–z), цифры и _, длина 3–32. Без пробелов и кириллицы.")
	}
	taken, err := repo.IsUsernameTaken(s.ctx(), s.DB, u)
	if err != nil {
		return err
	}
	if taken {
		return s.notifyWarn(chatID, "Такой логин уже занят. Придумайте другой и отправьте снова.")
	}
	dp := repo.EmptyPayload()
	dp.PendingRegUsername = strPtr(u)
	if err := repo.SaveDialogue(s.ctx(), s.DB, chatID, repo.FlowRegEmail, &dp); err != nil {
		return err
	}
	return s.notifyHTML(chatID, s.msgWithCancelHint(
		"<b>Шаг 2</b>: укажите <b>e-mail</b> — на него будут завязаны уведомления и восстановление.\n\n"+
			"Одна строка, формат как в обычной почте: <code>имя@домен</code>"),
		homeReplyKeyboardMarkup())
}

func (s *Service) handleRegEmail(chatID int64, sender *tele.User, payload repo.DialoguePayload, text string) error {
	if ok, err := s.alreadyLinkedChat(chatID, sender); err != nil || ok {
		return err
	}
	if payload.PendingRegUsername == nil || *payload.PendingRegUsername == "" {
		ep := repo.EmptyPayload()
		_ = repo.SaveDialogue(s.ctx(), s.DB, chatID, repo.FlowIdle, &ep)
		return s.notifyWarn(chatID, "Прервана регистрация или сессия устарела. Нажмите «Регистрация» снова или /start.")
	}
	email := strings.TrimSpace(text)
	if len(email) > 254 || !strings.Contains(email, "@") {
		return s.notifyWarn(chatID, "Похоже на неверный e-mail: нужен символ @ и домен. Проверьте опечатки.")
	}
	taken, err := repo.IsEmailTaken(s.ctx(), s.DB, email)
	if err != nil {
		return err
	}
	if taken {
		return s.notifyWarn(chatID, "Этот e-mail уже зарегистрирован. Нажмите «Войти», если это ваша почта, или укажите другую.")
	}
	dp := payload
	dp.PendingRegEmail = strPtr(email)
	if err := repo.SaveDialogue(s.ctx(), s.DB, chatID, repo.FlowRegPassword, &dp); err != nil {
		return err
	}
	return s.notifyHTML(chatID, s.msgWithCancelHint(
		"<b>Шаг 3</b>: придумайте <b>пароль</b> (от 8 символов, максимум 72) одним сообщением.\n\n"+
			"<i>После отправки сообщение с паролем будет удалено из чата.</i>"),
		homeReplyKeyboardMarkup())
}

func (s *Service) handleRegPassword(chatID int64, messageID int, sender *tele.User, payload repo.DialoguePayload, text string) error {
	if ok, err := s.alreadyLinkedChat(chatID, sender); err != nil || ok {
		return err
	}
	if payload.PendingRegUsername == nil || payload.PendingRegEmail == nil {
		ep := repo.EmptyPayload()
		_ = repo.SaveDialogue(s.ctx(), s.DB, chatID, repo.FlowIdle, &ep)
		return s.notifyWarn(chatID, "Прервана регистрация или сессия устарела. Нажмите «Регистрация» снова или /start.")
	}
	raw := strings.TrimSpace(text)
	if raw != "" && messageID > 0 {
		s.redactPasswordMessage(chatID, messageID, raw)
	}
	if len(raw) < 8 || len(raw) > 72 {
		return s.notifyWarn(chatID, "Пароль должен быть от 8 до 72 символов. Выберите другой и отправьте одним сообщением.")
	}
	pwdHash, err := bcrypt.GenerateFromPassword([]byte(raw), 10)
	if err != nil {
		return err
	}
	code, err := otpPlain()
	if err != nil {
		return err
	}
	otpHash, err := bcrypt.GenerateFromPassword([]byte(code), 10)
	if err != nil {
		return err
	}

	dp := payload
	dp.PendingRegPwdHash = strPtr(string(pwdHash))
	dp.PendingRegOTPHash = strPtr(string(otpHash))
	if err := repo.SaveDialogue(s.ctx(), s.DB, chatID, repo.FlowRegOtp, &dp); err != nil {
		return err
	}
	_ = s.notifyHTML(chatID, s.msgWithCancelHint(fmt.Sprintf(
		"<b>Шаг 4</b>: подтвердите регистрацию кодом из этого чата (действует <b>%d мин.</b>).\n"+
			"Введите шесть цифр отдельным сообщением.\n\n<code>%s</code>",
		otpMinutes, escHTML(code),
	)), homeReplyKeyboardMarkup())
	return nil
}

func (s *Service) handleRegOTP(chatID int64, sender *tele.User, payload repo.DialoguePayload, code string) error {
	if ok, err := s.alreadyLinkedChat(chatID, sender); err != nil || ok {
		return err
	}
	if payload.PendingRegUsername == nil || payload.PendingRegEmail == nil ||
		payload.PendingRegPwdHash == nil || payload.PendingRegOTPHash == nil {
		ep := repo.EmptyPayload()
		_ = repo.SaveDialogue(s.ctx(), s.DB, chatID, repo.FlowIdle, &ep)
		return s.notifyWarn(chatID, "Прервана регистрация или сессия устарела. Нажмите «Регистрация» снова или /start.")
	}
	if len(strings.TrimSpace(code)) != 6 {
		return s.notifyWarn(chatID, "Нужны ровно 6 цифр кода, как в сообщении бота выше.")
	}
	if err := bcrypt.CompareHashAndPassword([]byte(*payload.PendingRegOTPHash), []byte(strings.TrimSpace(code))); err != nil {
		_ = s.notifyWarn(chatID, "Код не подошёл. Проверьте цифры или запросите новую регистрацию: /cancel и снова «Регистрация».")
		return nil
	}

	uname := *payload.PendingRegUsername
	email := *payload.PendingRegEmail
	pwdHash := *payload.PendingRegPwdHash

	uid, err := repo.RegisterNewUser(s.ctx(), s.DB, uname, email, pwdHash)
	if err != nil {
		if errors.Is(err, repo.ErrDuplicate) {
			_ = s.notifyWarn(chatID, "Логин или почта уже заняты. Начните заново с /start.")
			ep := repo.EmptyPayload()
			_ = repo.SaveDialogue(s.ctx(), s.DB, chatID, repo.FlowIdle, &ep)
			return nil
		}
		return err
	}
	tgU := sender.Username
	_ = repo.BindTelegram(s.ctx(), s.DB, uid, telegramUserID(sender), ptrStrOrNil(tgU))
	_ = repo.InsertAuthLog(s.ctx(), s.DB, &uid, uname, "telegram-bot-register", true, strPtr("registered_and_linked"))

	_ = repo.ClearDialogue(s.ctx(), s.DB, chatID)
	return s.sendHomeMenu(chatID, telegramUserID(sender), "✅ <b>Аккаунт создан и привязан.</b> Добро пожаловать!")
}
