package bot

import (
	"strings"

	"launcher-backend/internal/repo"
	"launcher-backend/internal/telegram"

	tele "gopkg.in/telebot.v3"
)

func (s *Service) Attach(bot *tele.Bot) {
	bot.Handle(tele.OnText, func(c tele.Context) error {
		if err := s.HandleText(c); err != nil && s.Log != nil {
			s.Log.Error("handler", "err", err)
		}
		return nil
	})
	bot.Handle(tele.OnCallback, func(c tele.Context) error {
		if err := s.HandleCallback(c); err != nil && s.Log != nil {
			s.Log.Error("callback", "err", err)
		}
		return nil
	})
}

func (s *Service) HandleText(c tele.Context) error {
	msg := c.Message()
	chat := c.Chat()
	sender := c.Sender()
	if msg == nil || chat == nil || sender == nil {
		return nil
	}
	if chat.Type != tele.ChatPrivate {
		return nil
	}
	chatID := chat.ID
	msgID := msg.ID
	text := strings.TrimSpace(c.Text())

	if text == "/cancel" || strings.EqualFold(text, "отмена") {
		return s.onCancel(chatID, sender)
	}
	if text == "/start" || text == "/menu" || text == menuButtonLabel {
		return s.welcome(chatID, telegramUserID(sender))
	}
	if text == "/help" {
		return s.cmdHelp(chatID, telegramUserID(sender))
	}
	if text == "/donate" || text == donateKeyboardLabel {
		return s.replyDonateShop(chatID, telegramUserID(sender))
	}
	if text == "/launcher" || text == launcherKeyboardLabel {
		return s.replyLauncherDownload(chatID, telegramUserID(sender))
	}

	flow, payload, err := repo.ReadDialogue(s.ctx(), s.DB, chatID)
	if err != nil {
		return err
	}

	if text == "/2fa" || text == "/totp" {
		switch flow {
		case repo.FlowTotpConfirm, repo.FlowTotpDisablePwd, repo.FlowTotpDisableOTP:
			// пользователь в шаге 2FA — не перезапускаем сценарий
		default:
			if flow != repo.FlowIdle {
				_ = repo.ClearDialogue(s.ctx(), s.DB, chatID)
			}
			return s.beginTotpFlow(chatID, telegramUserID(sender))
		}
	}
	adm, err := s.resolveAdmin(telegramUserID(sender))
	if err != nil {
		return err
	}

	switch flow {
	case repo.FlowIdle:
		return s.idleActions(chatID, sender, telegramUserID(sender), text, adm)
	case repo.FlowLinkLogin:
		return s.handleLinkLogin(chatID, text)
	case repo.FlowLinkPassword:
		_, err := s.handleLinkPassword(chatID, msgID, sender, payload, text)
		return err
	case repo.FlowLinkOtp:
		return s.handleLinkOTP(chatID, sender, payload, strings.TrimSpace(text))
	case repo.FlowRegUsername:
		return s.handleRegUsername(chatID, sender, text)
	case repo.FlowRegEmail:
		return s.handleRegEmail(chatID, sender, payload, text)
	case repo.FlowRegPassword:
		return s.handleRegPassword(chatID, msgID, sender, payload, text)
	case repo.FlowRegOtp:
		return s.handleRegOTP(chatID, sender, payload, strings.TrimSpace(text))
	case repo.FlowChangePwdOld:
		return s.handlePasswordOld(chatID, msgID, telegramUserID(sender), text, payload)
	case repo.FlowChangePwdWaitOtp:
		return s.handlePasswordAfterOTP(chatID, telegramUserID(sender), text, payload)
	case repo.FlowChangePwdNew:
		return s.handlePasswordNew(chatID, msgID, telegramUserID(sender), text, payload)
	case repo.FlowChangeEmailAsk:
		_, err := s.handleChangeEmailAsk(chatID, telegramUserID(sender), payload, text)
		return err
	case repo.FlowChangeEmailWaitOtp:
		return s.handleChangeEmailOTP(chatID, telegramUserID(sender), text, payload)
	case repo.FlowTotpConfirm:
		return s.handleTotpConfirm(chatID, msgID, telegramUserID(sender), text)
	case repo.FlowTotpDisablePwd:
		return s.handleTotpDisablePwd(chatID, msgID, telegramUserID(sender), text)
	case repo.FlowTotpDisableOTP:
		return s.handleTotpDisableOTP(chatID, msgID, telegramUserID(sender), text)
	case repo.FlowAdminMenu:
		if adm == nil {
			_ = s.notifyWarn(chatID, "Панель администратора вам недоступна (роль или список модераторов). Нажмите /start для обычного меню.")
			ep := repo.EmptyPayload()
			return repo.SaveDialogue(s.ctx(), s.DB, chatID, repo.FlowIdle, &ep)
		}
		return s.adminMenuActions(chatID, telegramUserID(sender), text, adm)
	case repo.FlowAdminSearch:
		return s.adminSearch(chatID, text, adm)
	case repo.FlowAdminAwaitPick:
		return s.adminPick(chatID, text, payload, adm)
	case repo.FlowAdminManaging:
		return s.adminManage(chatID, telegramUserID(sender), text)
	case repo.FlowAdminAskNewEmail:
		return s.adminApplyEmail(chatID, telegramUserID(sender), strings.TrimSpace(text))
	default:
		_ = s.notifyWarn(chatID, "Сейчас бот ждёт другой тип ответа или состояние сбилось. Попробуйте /cancel, затем /start.")
		return nil
	}
}

func (s *Service) onCancel(chatID int64, sender *tele.User) error {
	_ = repo.ClearDialogue(s.ctx(), s.DB, chatID)
	return s.sendHomeMenu(chatID, telegramUserID(sender), "Сценарий сброшен — вы в главном меню.")
}

func (s *Service) welcome(chatID int64, telegramUID int64) error {
	_ = repo.ClearDialogue(s.ctx(), s.DB, chatID)
	// Меню несёт inline-клавиатуру; persistent-кнопка «🏠 Меню» выставится
	// первым же обычным сообщением (подсказка сценария, /help и т.п.).
	return s.sendHomeMenu(chatID, telegramUID, "")
}

func (s *Service) idleActions(chatID int64, _tg *tele.User, telegramUID int64, text string, adminOpt *adminContext) error {
	switch text {
	case "/admin":
		if adminOpt != nil {
			ep := repo.EmptyPayload()
			_ = repo.SaveDialogue(s.ctx(), s.DB, chatID, repo.FlowAdminMenu, &ep)
			return s.notifyHTML(chatID,
				"<b>Панель администратора</b>\n"+
					"• «🔍 Поиск» — найти игрока по нику, логину, почте или id.\n"+
					"• «📡 OPS» — краткий дайджест сервисов (если настроено в .env).\n"+
					"• «⬅ Выйти» — вернуться к обычному меню.\n\n"+
					"Доступ только у ролей admin/moderator и при вашем id в allowlist (если список задан).",
				s.adminOpsKeyboardMarkup())
		}
		return s.notifyWarn(chatID, "Админка недоступна: нужна роль admin или moderator в базе и, если задан ADMIN_TELEGRAM_IDS, ваш Telegram id должен входить в него.")

	case "Профиль", "/profile":
		return s.profileCard(chatID, telegramUID)

	case "🔑 Войти", "/bind", "/login":
		return s.beginLoginFlow(chatID, _tg)

	case "📋 Регистрация", "/register":
		return s.beginRegisterFlow(chatID, _tg)

	case "Сменить пароль", "/password":
		return s.beginPasswordFlow(chatID, telegramUID)

	case "Email", "/email":
		return s.beginEmailFlow(chatID, telegramUID)

	case "2FA", "/2fa", "/totp":
		return s.beginTotpFlow(chatID, telegramUID)

	case "🛠 Админка":
		if adminOpt != nil {
			ep := repo.EmptyPayload()
			_ = repo.SaveDialogue(s.ctx(), s.DB, chatID, repo.FlowAdminMenu, &ep)
			return s.notifyHTML(chatID,
				"<b>Админка</b>\nВыберите действие на клавиатуре. Быстрый вход: /admin.\n"+
					"<i>Обычные игроки этот раздел не видят.</i>",
				s.adminOpsKeyboardMarkup())
		}
		return s.notifyWarn(chatID, "Эта панель только для модераторов. Вам доступны кнопки «Войти», «Регистрация» и после привязки — профиль, пароль, почта, 2FA.")

	case "/ops":
		if adminOpt != nil {
			d := opsFormatDigest(s.HTTP, s.Cfg)
			return s.notifyHTML(chatID, "<pre>"+escHTML(d)+"</pre>", nil)
		}
		return s.notifyWarn(chatID, "/ops доступна только модераторам. Список команд для игроков: /help.")

	default:
		return s.notifyWarn(chatID, "Не распознал сообщение.\nНажмите «🏠 Меню» или /start.")
	}
}

func (s *Service) profileCard(chatID int64, telegramUID int64) error {
	me, err := repo.FindUserByTelegram(s.ctx(), s.DB, telegramUID)
	if err != nil {
		return err
	}
	if me == nil {
		return s.notifyWarn(chatID, "Профиль ещё не привязан к Telegram.\n\n"+
			"• Есть аккаунт — нажмите «🔑 Войти» и пройдите проверку паролем.\n"+
			"• Нет аккаунта — «📋 Регистрация».")
	}
	v, err := s.menuViewFor(telegramUID)
	if err != nil {
		return err
	}
	text, markup := buildProfileScreen(v)
	if old, err2 := repo.ReadMenuMessage(s.ctx(), s.DB, chatID); err2 == nil && old > 0 {
		_ = telegram.DeleteMessage(s.HTTP, s.Cfg.TelegramBotToken, chatID, old)
	}
	id, err := telegram.SendMessageHTMLWithID(s.HTTP, s.Cfg.TelegramBotToken, chatID, text, markup, s.bannerPreview())
	if err != nil {
		return err
	}
	return repo.SaveMenuMessage(s.ctx(), s.DB, chatID, id)
}

func (s *Service) cmdHelp(chatID int64, _ int64) error {
	kb := homeReplyKeyboardMarkup()
	txt := strings.Join([]string{
		"<b>Справка по боту</b>",
		"",
		"<b>Базовые команды</b>",
		"<code>/start</code> или «🏠 Меню» — показать живое меню",
		"<code>/menu</code> — показать живое меню",
		"<code>/help</code> — этот текст",
		"<code>/cancel</code> — отменить текущий шаг (ввод логина, пароля, кода и т.д.)",
		"",
		"<i>Нажмите кнопку «🏠 Меню» в нижней клавиатуре — откроется главный экран с кнопками навигации.</i>",
		"",
		"<b>Аккаунт</b>",
		"<code>/profile</code> — ваш профиль (UUID, логин, ник, 2FA)",
		"<code>/bind</code> или «Войти» — связать Telegram с уже существующей учёткой",
		"<code>/register</code> — новая учётка: логин сайта, почта, пароль, подтверждение кодом",
		"<code>/password</code> — смена пароля (нужна привязка)",
		"<code>/email</code> — смена почты: новый e-mail → код из чата",
		"<code>/2fa</code> — включить или выключить двухфакторку для <b>лаунчера</b>",
		"<code>/donate</code> — ссылка на магазин и донат",
		"<code>/launcher</code> — получить файл лаунчера в чат (если настроен LAUNCHER_EXE_PATH)",
		"<code>/admin</code> — панель модератора (только при роли и allowlist)",
	}, "\n")
	return s.notifyHTML(chatID, txt, kb)
}

// beginPasswordFlow запускает сценарий смены пароля (текстовые шаги как раньше).
func (s *Service) beginPasswordFlow(chatID, telegramUID int64) error {
	if uidPtr, err := s.linkedUID(telegramUID); err != nil {
		return err
	} else if uidPtr == nil {
		return s.forbidNotLinked(chatID, telegramUID)
	}
	ep := repo.EmptyPayload()
	_ = repo.SaveDialogue(s.ctx(), s.DB, chatID, repo.FlowChangePwdOld, &ep)
	return s.notifyHTML(chatID, s.msgWithCancelHint(
		"Смена пароля в два шага:\n"+
			"1) Сейчас пришлите <b>текущий пароль</b> (его сообщение будет удалено из чата и заменено заглушкой).\n"+
			"2) Затем бот пришлёт <b>шестизначный код</b> в этот чат — введите его.\n"+
			"3) После этого пришлите <b>новый пароль</b> (8–128 символов).\n\n"+
			"<i>Если забыли пароль — обратитесь к администратору проекта.</i>"),
		homeReplyKeyboardMarkup())
}

// beginEmailFlow запускает сценарий смены почты.
func (s *Service) beginEmailFlow(chatID, telegramUID int64) error {
	if uidPtr, err := s.linkedUID(telegramUID); err != nil {
		return err
	} else if uidPtr == nil {
		return s.forbidNotLinked(chatID, telegramUID)
	}
	ep := repo.EmptyPayload()
	_ = repo.SaveDialogue(s.ctx(), s.DB, chatID, repo.FlowChangeEmailAsk, &ep)
	return s.notifyHTML(chatID, s.msgWithCancelHint(
		"Укажите новый <b>адрес e-mail</b> одним сообщением (тот, который будет храниться в аккаунте).\n\n"+
			"После этого бот пришлёт код в чат — его нужно будет ввести, чтобы подтвердить смену."),
		homeReplyKeyboardMarkup())
}
