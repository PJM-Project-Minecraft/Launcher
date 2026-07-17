package bot

import (
	"fmt"
	"strconv"
	"strings"

	"launcher-backend/internal/repo"
	"launcher-backend/internal/telegram"

	tele "gopkg.in/telebot.v3"
)

const supportMaxLen = 2000

// beginSupportFlow — игрок нажал «🆘 Поддержка»: просим описать вопрос одним
// сообщением. Требует привязанный аккаунт (кому отвечать) и принятую политику.
func (s *Service) beginSupportFlow(chatID, telegramUID int64) error {
	if uidPtr, err := s.linkedUID(telegramUID); err != nil {
		return err
	} else if uidPtr == nil {
		return s.forbidNotLinked(chatID, telegramUID)
	}
	if blocked, err := s.policyGateText(chatID, telegramUID); err != nil || blocked {
		return err
	}
	ep := repo.EmptyPayload()
	_ = repo.SaveDialogue(s.ctx(), s.DB, chatID, repo.FlowSupportMsg, &ep)
	return s.notifyHTML(chatID, s.msgWithCancelHint(
		"🆘 <b>Поддержка</b>\n\n"+
			"Опишите вопрос или проблему <b>одним сообщением</b> — передадим команде проекта. "+
			"Ответ придёт прямо в этот чат.\n\n"+
			"<i>Не пишите пароли — для их сброса есть «Пароль» → «🆘 Забыл пароль».</i>"),
		keyboardDismiss())
}

// handleSupportMessage — игрок прислал текст обращения: создаём/дополняем тикет
// и уведомляем админов.
func (s *Service) handleSupportMessage(chatID, telegramUID int64, text string) error {
	u, err := repo.FindUserByTelegram(s.ctx(), s.DB, telegramUID)
	if err != nil {
		return err
	}
	if u == nil {
		_ = repo.ClearDialogue(s.ctx(), s.DB, chatID)
		return s.forbidNotLinked(chatID, telegramUID)
	}
	msg := strings.TrimSpace(text)
	if msg == "" {
		return s.notifyWarn(chatID, "Пустое сообщение. Опишите вопрос текстом или отмените: /cancel.")
	}
	if len(msg) > supportMaxLen {
		return s.notifyWarn(chatID, fmt.Sprintf("Слишком длинно (макс. %d символов). Сократите и отправьте снова.", supportMaxLen))
	}
	id, created, err := repo.CreateOrAppendSupport(s.ctx(), s.DB, u.ID, chatID, msg)
	if err != nil {
		return err
	}
	s.notifyAdminsSupport(id, u.Login, u.TelegramUsername, msg, created)

	_ = repo.ClearDialogue(s.ctx(), s.DB, chatID)
	return s.sendHomeMenu(chatID, telegramUID,
		"✅ <b>Обращение отправлено.</b> Команда проекта ответит прямо в этот чат.")
}

// supportAdminCard — карточка тикета для админа.
func supportAdminCard(id uint, login, tgUsername, msg string, created bool) string {
	tg := "—"
	if strings.TrimSpace(tgUsername) != "" {
		tg = "@" + escHTML(strings.TrimPrefix(strings.TrimSpace(tgUsername), "@"))
	}
	head := "🆘 <b>Тикет #%d: поддержка</b>"
	if !created {
		head = "🆘 <b>Тикет #%d: новое сообщение</b>"
	}
	return fmt.Sprintf(head+"\n\n"+
		"👤 Игрок: <b>%s</b> (%s)\n\n"+
		"💬 %s\n\n"+
		"<i>«Ответить» — следующим сообщением бот доставит текст игроку.</i>",
		id, escHTML(login), tg, escHTML(msg))
}

func supportAdminMarkup(id uint) map[string]any {
	sid := strconv.FormatUint(uint64(id), 10)
	return telegram.InlineMarkup([]telegram.InlineBtn{
		{Text: "✍ Ответить", Data: "sup:reply:" + sid},
		{Text: "✅ Закрыть", Data: "sup:close:" + sid},
	})
}

// notifyAdminsSupport рассылает тикет всем допущенным админам/модераторам.
func (s *Service) notifyAdminsSupport(id uint, login, tgUsername, msg string, created bool) {
	admins, err := repo.ListPrivilegedWithTelegram(s.ctx(), s.DB)
	if err != nil {
		if s.Log != nil {
			s.Log.Error("support: список админов", "err", err)
		}
		return
	}
	card := supportAdminCard(id, login, tgUsername, msg, created)
	markup := supportAdminMarkup(id)
	for _, a := range admins {
		if a.TelegramID == nil || !s.Cfg.AdminAllowlisted(*a.TelegramID) {
			continue
		}
		if err := s.notifyHTML(*a.TelegramID, card, markup); err != nil && s.Log != nil {
			s.Log.Warn("support: уведомление админа", "tg", *a.TelegramID, "err", err)
		}
	}
}

// handleSupportAction — админ нажал «Ответить»/«Закрыть» (sup:reply:<id> / sup:close:<id>).
func (s *Service) handleSupportAction(cb *tele.Callback, chatID, telegramUID int64, msgID int, data string) error {
	adm, err := s.resolveAdmin(telegramUID)
	if err != nil {
		s.answerCb(cb.ID, "Ошибка, попробуйте ещё раз", false)
		return err
	}
	if adm == nil {
		s.answerCb(cb.ID, "Тикеты обрабатывают только администраторы.", true)
		return nil
	}

	rest, isClose := "", false
	switch {
	case strings.HasPrefix(data, "sup:reply:"):
		rest = strings.TrimPrefix(data, "sup:reply:")
	case strings.HasPrefix(data, "sup:close:"):
		rest, isClose = strings.TrimPrefix(data, "sup:close:"), true
	default:
		s.answerCb(cb.ID, "", false)
		return nil
	}
	id64, err := strconv.ParseUint(rest, 10, 64)
	if err != nil {
		s.answerCb(cb.ID, "Некорректный тикет.", true)
		return nil
	}
	ticketID := uint(id64)

	t, err := repo.GetSupportTicket(s.ctx(), s.DB, ticketID)
	if err != nil {
		return err
	}
	if t == nil {
		s.answerCb(cb.ID, "Тикет не найден.", true)
		return nil
	}

	if isClose {
		ok, err := repo.CloseSupportTicket(s.ctx(), s.DB, ticketID)
		if err != nil {
			return err
		}
		if !ok {
			s.answerCb(cb.ID, "Тикет уже закрыт.", true)
		} else {
			s.answerCb(cb.ID, "Тикет закрыт", false)
			_ = s.notifyHTML(t.ChatID,
				"✅ Ваше обращение в поддержку <b>закрыто</b>. Если вопрос остался — откройте новое из меню.",
				keyboardDismiss())
		}
		s.markSupportMessage(chatID, msgID, ticketID, "✅ закрыт")
		return nil
	}

	// «Ответить»: переводим чат админа в режим ввода ответа для этого тикета.
	dp := repo.EmptyPayload()
	dp.SupportTicketID = &ticketID
	if err := repo.SaveDialogue(s.ctx(), s.DB, chatID, repo.FlowSupportReply, &dp); err != nil {
		return err
	}
	s.answerCb(cb.ID, "", false)
	return s.notifyHTML(chatID, s.msgWithCancelHint(fmt.Sprintf(
		"✍ Ответ по тикету <b>#%d</b>. Напишите текст одним сообщением — бот доставит игроку.",
		ticketID)), keyboardDismiss())
}

// handleSupportReply — админ прислал текст ответа: доставляем игроку.
func (s *Service) handleSupportReply(chatID, telegramUID int64, payload repo.DialoguePayload, text string) error {
	if adm, err := s.resolveAdmin(telegramUID); err != nil {
		return err
	} else if adm == nil {
		_ = repo.ClearDialogue(s.ctx(), s.DB, chatID)
		return s.notifyWarn(chatID, "Ответы на тикеты доступны только администраторам.")
	}
	if payload.SupportTicketID == nil {
		_ = repo.ClearDialogue(s.ctx(), s.DB, chatID)
		return s.notifyWarn(chatID, "Не понял, по какому тикету ответ. Откройте карточку тикета и нажмите «Ответить».")
	}
	t, err := repo.GetSupportTicket(s.ctx(), s.DB, *payload.SupportTicketID)
	if err != nil {
		return err
	}
	if t == nil {
		_ = repo.ClearDialogue(s.ctx(), s.DB, chatID)
		return s.notifyWarn(chatID, "Тикет не найден (возможно, удалён).")
	}
	reply := strings.TrimSpace(text)
	if reply == "" {
		return s.notifyWarn(chatID, "Пустой ответ. Напишите текст или отмените: /cancel.")
	}
	if len(reply) > supportMaxLen {
		return s.notifyWarn(chatID, fmt.Sprintf("Слишком длинно (макс. %d символов).", supportMaxLen))
	}

	deliver := "🆘 <b>Ответ поддержки</b>\n\n" + escHTML(reply) + "\n\n" +
		"<i>Нужно уточнить — нажмите «Ответить».</i>"
	markup := telegram.InlineMarkup([]telegram.InlineBtn{{Text: "✍ Ответить", Data: cbSupport}})
	if err := s.notifyHTML(t.ChatID, deliver, markup); err != nil {
		if s.Log != nil {
			s.Log.Error("support: доставка ответа игроку", "chat", t.ChatID, "err", err)
		}
		return s.notifyWarn(chatID, "Не удалось доставить ответ игроку — свяжитесь с ним вручную.")
	}
	_ = repo.ClearDialogue(s.ctx(), s.DB, chatID)
	return s.notifyHTML(chatID, fmt.Sprintf("✅ Ответ по тикету <b>#%d</b> доставлен игроку.", t.ID), keyboardDismiss())
}

// markSupportMessage заменяет карточку тикета у админа итогом (снимает кнопки).
func (s *Service) markSupportMessage(chatID int64, msgID int, ticketID uint, result string) {
	text := fmt.Sprintf("🆘 Тикет #%d: %s.", ticketID, result)
	if err := telegram.EditMessageTextHTML(s.HTTP, s.Cfg.TelegramBotToken, chatID, msgID, text, nil, nil); err != nil && s.Log != nil {
		s.Log.Warn("support: правка карточки", "err", err)
	}
}
