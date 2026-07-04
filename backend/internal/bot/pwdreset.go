package bot

import (
	"fmt"
	"strconv"
	"strings"

	"launcher-backend/internal/models"
	"launcher-backend/internal/repo"
	"launcher-backend/internal/telegram"

	"golang.org/x/crypto/bcrypt"
	tele "gopkg.in/telebot.v3"
)

// requestPwdReset — игрок нажал «🆘 Забыл пароль»: создаём заявку (с дедупом),
// уведомляем админов inline-кнопками и показываем игроку статус на месте меню.
func (s *Service) requestPwdReset(cb *tele.Callback, chatID int64, msgID int, v menuView) error {
	if v.User == nil {
		s.answerCb(cb.ID, "Сначала привяжите аккаунт.", true)
		return nil
	}
	id, created, err := repo.CreatePwdReset(s.ctx(), s.DB, v.User.ID, chatID)
	if err != nil {
		s.answerCb(cb.ID, "Не получилось отправить заявку, попробуйте ещё раз.", true)
		return err
	}
	if !created {
		s.answerCb(cb.ID, "Заявка уже на рассмотрении — дождитесь решения администратора.", true)
		return nil
	}
	s.answerCb(cb.ID, "Заявка отправлена ✅", false)
	s.notifyAdminsPwdReset(id, v.User)

	text := "🆘 <b>Заявка отправлена</b>\n\n" +
		"Администратор рассмотрит её и, если одобрит, бот пришлёт " +
		"<b>новый пароль</b> прямо в этот чат.\n\n" +
		"<i>Обычно это занимает немного времени — можно закрыть чат и вернуться позже.</i>"
	markup := telegram.InlineMarkup(backRow())
	return s.editMenuScreen(chatID, msgID, text, markup)
}

// pwdResetAdminCard — текст уведомления/карточки заявки для администратора.
func pwdResetAdminCard(id uint, u *models.User) string {
	tg := "—"
	if strings.TrimSpace(u.TelegramUsername) != "" {
		tg = "@" + escHTML(strings.TrimPrefix(strings.TrimSpace(u.TelegramUsername), "@"))
	}
	return fmt.Sprintf(
		"🆘 <b>Заявка #%d: сброс пароля</b>\n\n"+
			"👤 Игрок: <b>%s</b> (%s)\n"+
			"📧 Почта: %s\n\n"+
			"<i>«Выдать» — бот сгенерирует пароль и пришлёт игроку в чат.</i>",
		id, escHTML(u.Login), tg, escHTML(maskEmailUnsafe(u.Email)))
}

func pwdResetAdminMarkup(id uint) map[string]any {
	sid := strconv.FormatUint(uint64(id), 10)
	return telegram.InlineMarkup([]telegram.InlineBtn{
		{Text: "✅ Выдать пароль", Data: "pr:ok:" + sid},
		{Text: "❌ Отклонить", Data: "pr:no:" + sid},
	})
}

// notifyAdminsPwdReset рассылает заявку всем допущенным админам/модераторам
// с привязанным Telegram. Ошибки отдельных доставок — только в лог.
func (s *Service) notifyAdminsPwdReset(id uint, u *models.User) {
	admins, err := repo.ListPrivilegedWithTelegram(s.ctx(), s.DB)
	if err != nil {
		if s.Log != nil {
			s.Log.Error("pwd-reset: список админов", "err", err)
		}
		return
	}
	card := pwdResetAdminCard(id, u)
	markup := pwdResetAdminMarkup(id)
	sent := 0
	for _, a := range admins {
		if a.TelegramID == nil || !s.Cfg.AdminAllowlisted(*a.TelegramID) {
			continue
		}
		if err := s.notifyHTML(*a.TelegramID, card, markup); err != nil {
			if s.Log != nil {
				s.Log.Warn("pwd-reset: уведомление админа", "tg", *a.TelegramID, "err", err)
			}
			continue
		}
		sent++
	}
	if s.Log != nil {
		s.Log.Info("pwd-reset: заявка создана", "id", id, "login", u.Login, "admins", sent)
	}
}

// handlePwdResetDecision — админ нажал «Выдать»/«Отклонить» (pr:ok:<id> / pr:no:<id>).
func (s *Service) handlePwdResetDecision(cb *tele.Callback, chatID, telegramUID int64, msgID int, data string) error {
	adm, err := s.resolveAdmin(telegramUID)
	if err != nil {
		s.answerCb(cb.ID, "Ошибка, попробуйте ещё раз", false)
		return err
	}
	if adm == nil {
		s.answerCb(cb.ID, "Решения по заявкам принимают только администраторы.", true)
		return nil
	}

	rest, approve := "", false
	switch {
	case strings.HasPrefix(data, "pr:ok:"):
		rest, approve = strings.TrimPrefix(data, "pr:ok:"), true
	case strings.HasPrefix(data, "pr:no:"):
		rest, approve = strings.TrimPrefix(data, "pr:no:"), false
	default:
		s.answerCb(cb.ID, "", false)
		return nil
	}
	id64, err := strconv.ParseUint(rest, 10, 64)
	if err != nil {
		s.answerCb(cb.ID, "Некорректная заявка.", true)
		return nil
	}
	reqID := uint(id64)

	req, err := repo.GetPwdReset(s.ctx(), s.DB, reqID)
	if err != nil {
		return err
	}
	if req == nil {
		s.answerCb(cb.ID, "Заявка не найдена.", true)
		return nil
	}
	target, err := repo.FindUserByID(s.ctx(), s.DB, req.UserID)
	if err != nil {
		return err
	}
	if target == nil {
		s.answerCb(cb.ID, "Аккаунт заявки уже удалён.", true)
		_, _ = repo.DecidePwdReset(s.ctx(), s.DB, reqID, models.PwdResetRejected, adm.user.Login)
		return nil
	}
	// Модератор не решает заявки аккаунтов с ролью не ниже своей (антиэскалация).
	if roleRank(target.Role) >= roleRank(adm.user.Role) {
		s.answerCb(cb.ID, "Недостаточно прав для этого аккаунта.", true)
		return nil
	}

	status := models.PwdResetRejected
	if approve {
		status = models.PwdResetApproved
	}
	ok, err := repo.DecidePwdReset(s.ctx(), s.DB, reqID, status, adm.user.Login)
	if err != nil {
		return err
	}
	if !ok {
		s.answerCb(cb.ID, "Заявка уже обработана другим администратором.", true)
		s.markPwdResetMessage(chatID, msgID, reqID, target, "обработана ранее")
		return nil
	}

	if !approve {
		s.answerCb(cb.ID, "Заявка отклонена", false)
		_ = s.notifyHTML(req.ChatID,
			"❌ Заявка на сброс пароля <b>отклонена</b> администратором.\n"+
				"Если это ошибка — свяжитесь с командой проекта.", homeReplyKeyboardMarkup())
		s.markPwdResetMessage(chatID, msgID, reqID, target, "❌ отклонена")
		td := telegramUID
		_ = repo.InsertAudit(s.ctx(), s.DB, &td, nil, &req.UserID, "pwd_reset_rejected", nil)
		return nil
	}

	pwd, err := randPassword14()
	if err != nil {
		return err
	}
	hash, err := bcrypt.GenerateFromPassword([]byte(pwd), 10)
	if err != nil {
		return err
	}
	if err := repo.SetPassword(s.ctx(), s.DB, req.UserID, string(hash)); err != nil {
		return err
	}
	td := telegramUID
	_ = repo.InsertAudit(s.ctx(), s.DB, &td, nil, &req.UserID, "pwd_reset_approved", strPtr("hash_rotated"))

	if err := s.notifyHTML(req.ChatID,
		"✅ <b>Заявка одобрена — вот новый пароль</b> (нажмите, чтобы показать):\n\n"+
			"<tg-spoiler><code>"+escHTML(pwd)+"</code></tg-spoiler>\n\n"+
			"<i>Войдите с ним в лаунчер и при желании смените в разделе «Пароль».</i>",
		homeReplyKeyboardMarkup()); err != nil {
		if s.Log != nil {
			s.Log.Error("pwd-reset: отправка пароля игроку", "chat", req.ChatID, "err", err)
		}
		s.answerCb(cb.ID, "Пароль сменён, но доставить игроку не удалось — свяжитесь с ним вручную.", true)
		s.markPwdResetMessage(chatID, msgID, reqID, target, "⚠ выдан, но не доставлен")
		return nil
	}
	s.answerCb(cb.ID, "Пароль выдан ✅", false)
	s.markPwdResetMessage(chatID, msgID, reqID, target, "✅ пароль выдан, отправлен игроку")
	return nil
}

// markPwdResetMessage заменяет карточку заявки у админа итогом (и снимает кнопки).
func (s *Service) markPwdResetMessage(chatID int64, msgID int, reqID uint, u *models.User, result string) {
	text := fmt.Sprintf("🆘 Заявка #%d (<b>%s</b>): %s.", reqID, escHTML(u.Login), result)
	if err := telegram.EditMessageTextHTML(s.HTTP, s.Cfg.TelegramBotToken, chatID, msgID, text, nil, nil); err != nil && s.Log != nil {
		s.Log.Warn("pwd-reset: правка карточки", "err", err)
	}
}
