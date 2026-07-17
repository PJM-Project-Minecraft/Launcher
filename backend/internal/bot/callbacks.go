package bot

import (
	"strings"

	"launcher-backend/internal/policy"
	"launcher-backend/internal/telegram"

	tele "gopkg.in/telebot.v3"
)

// normalizeCallbackData срезает служебный префикс "\f" telebot и пробелы.
func normalizeCallbackData(raw string) string {
	return strings.TrimSpace(strings.TrimPrefix(raw, "\f"))
}

// platformForCallback — маппинг кнопки выбора платформы на внутренний код релиза.
func platformForCallback(data string) string {
	switch data {
	case cbLauncherLinux:
		return "linux-x64"
	case cbLauncherWindows:
		return "windows-x64"
	}
	return ""
}

// callbackNeedsLink — экраны, доступные только привязанному аккаунту.
// Решения по заявкам (pr:*) сюда не входят: у них своя админ-проверка.
func callbackNeedsLink(data string) bool {
	switch data {
	case cbProfile, cbPwd, cbPwdChange, cbPwdReset, cbEmail, cb2FA, cb2FAOn, cb2FAOff, cbAdmin, cbSupport:
		return true
	}
	return false
}

// answerCb снимает «часики»; ошибки только в лог — это не повод ронять обработку.
func (s *Service) answerCb(id, text string, alert bool) {
	if err := telegram.AnswerCallbackQuery(s.HTTP, s.Cfg.TelegramBotToken, id, text, alert); err != nil && s.Log != nil {
		s.Log.Warn("answerCallbackQuery", "err", err)
	}
}

// HandleCallback — диспетчер нажатий inline-кнопок меню.
func (s *Service) HandleCallback(c tele.Context) error {
	cb := c.Callback()
	if cb == nil || cb.Message == nil || c.Chat() == nil || c.Sender() == nil {
		return nil
	}
	chatID := c.Chat().ID
	telegramUID := telegramUserID(c.Sender())
	msgID := cb.Message.ID
	data := normalizeCallbackData(cb.Data)

	// Решения по заявкам на сброс пароля — отдельная админ-ветка (pr:ok:<id> / pr:no:<id>).
	if strings.HasPrefix(data, "pr:") {
		return s.handlePwdResetDecision(cb, chatID, telegramUID, msgID, data)
	}
	// Действия админа по тикетам поддержки (sup:reply:<id> / sup:close:<id>).
	if strings.HasPrefix(data, "sup:") {
		return s.handleSupportAction(cb, chatID, telegramUID, msgID, data)
	}

	v, err := s.menuViewFor(telegramUID)
	if err != nil {
		s.answerCb(cb.ID, "Ошибка, попробуйте ещё раз", false)
		return err
	}

	if callbackNeedsLink(data) && v.User == nil {
		s.answerCb(cb.ID, "Сначала привяжите аккаунт: «Войти» или «Регистрация».", true)
		text, markup := buildHomeScreen(v, "")
		return s.editMenuScreen(chatID, msgID, text, markup)
	}

	// Гейт политики: привязанный пользователь без актуального согласия
	// с любой кнопки попадает на экран политики.
	if policyGateApplies(v, data) {
		s.answerCb(cb.ID, "Сначала примите политику конфиденциальности.", false)
		text, markup := buildPolicyScreen(s.policyURL())
		return s.editMenuScreen(chatID, msgID, text, markup)
	}

	switch data {
	case cbHome:
		s.answerCb(cb.ID, "", false)
		text, markup := buildHomeScreen(v, "")
		return s.editMenuScreen(chatID, msgID, text, markup)

	case cbProfile:
		s.answerCb(cb.ID, "", false)
		text, markup := buildProfileScreen(v)
		return s.editMenuScreen(chatID, msgID, text, markup)

	case cb2FA:
		s.answerCb(cb.ID, "", false)
		text, markup := build2FAScreen(v)
		return s.editMenuScreen(chatID, msgID, text, markup)

	case cbDonate:
		s.answerCb(cb.ID, "", false)
		text, markup := buildDonateScreen(v)
		return s.editMenuScreen(chatID, msgID, text, markup)

	case cbLauncher:
		s.answerCb(cb.ID, "", false)
		text, markup := buildLauncherScreen(v)
		return s.editMenuScreen(chatID, msgID, text, markup)

	case cbLauncherFile:
		s.answerCb(cb.ID, "Отправляю файл…", false)
		return s.replyLauncherDownload(chatID, telegramUID)

	case cbLauncherLinux, cbLauncherWindows:
		platform := platformForCallback(data)
		s.answerCb(cb.ID, "Отправляю файл…", false)
		return s.replyLauncherReleaseDownload(chatID, platform)

	case cbPwd:
		s.answerCb(cb.ID, "", false)
		text, markup := buildPasswordScreen(v)
		return s.editMenuScreen(chatID, msgID, text, markup)

	case cbPwdChange:
		s.answerCb(cb.ID, "", false)
		return s.beginPasswordFlow(chatID, telegramUID)

	case cbPwdReset:
		return s.requestPwdReset(cb, chatID, msgID, v)

	case cbSupport:
		s.answerCb(cb.ID, "", false)
		return s.beginSupportFlow(chatID, telegramUID)

	case cbEmail:
		s.answerCb(cb.ID, "", false)
		return s.beginEmailFlow(chatID, telegramUID)

	case cb2FAOn, cb2FAOff:
		// beginTotpFlow сам перечитывает состояние — защита от протухшей кнопки.
		s.answerCb(cb.ID, "", false)
		return s.beginTotpFlow(chatID, telegramUID)

	case cbLogin:
		s.answerCb(cb.ID, "", false)
		return s.beginLoginFlow(chatID, c.Sender())

	case cbRegister:
		s.answerCb(cb.ID, "", false)
		return s.beginRegisterFlow(chatID, c.Sender())

	case cbAdmin:
		adm, err := s.resolveAdmin(telegramUID)
		if err != nil {
			s.answerCb(cb.ID, "Ошибка, попробуйте ещё раз", false)
			return err
		}
		if adm == nil {
			s.answerCb(cb.ID, "Панель только для модераторов.", true)
			return nil
		}
		s.answerCb(cb.ID, "", false)
		return s.adminPanelIntro(chatID)

	case cbPolicyAccept:
		if v.User == nil {
			s.answerCb(cb.ID, "Аккаунт не привязан.", true)
			text, markup := buildHomeScreen(v, "")
			return s.editMenuScreen(chatID, msgID, text, markup)
		}
		if err := policy.RecordConsent(s.ctx(), s.DB, v.User.ID, policy.SourceBot, ""); err != nil {
			s.answerCb(cb.ID, "Ошибка, попробуйте ещё раз", false)
			return err
		}
		s.answerCb(cb.ID, "Спасибо!", false)
		nv, err := s.menuViewFor(telegramUID)
		if err != nil {
			return err
		}
		text, markup := buildHomeScreen(nv, "✅ Политика конфиденциальности принята.")
		return s.editMenuScreen(chatID, msgID, text, markup)

	case cbPolicyRegAccept:
		s.answerCb(cb.ID, "", false)
		return s.startRegisterSteps(chatID, c.Sender())

	default:
		// Кнопка из старой версии меню — просто пересоздаём главный экран.
		s.answerCb(cb.ID, "Меню обновилось", false)
		return s.sendHomeMenu(chatID, telegramUID, "")
	}
}
