package bot

import (
	"fmt"
	"strings"
	"time"

	"launcher-backend/internal/models"
	"launcher-backend/internal/repo"

	"golang.org/x/crypto/bcrypt"
	tele "gopkg.in/telebot.v3"
)

// pwdResetCooldown — пауза между сбросами пароля одним игроком: сброс мгновенный,
// без админа, поэтому кнопку нельзя жать бесконечно (bcrypt на каждый клик).
const pwdResetCooldown = 5 * time.Minute

// requestPwdReset — игрок нажал «🆘 Забыл пароль»: сразу генерируем новый пароль
// и присылаем в этот чат. Подтверждение админа не требуется — доступ к чату уже
// доказан привязкой Telegram-аккаунта; админам уходит уведомление постфактум.
func (s *Service) requestPwdReset(cb *tele.Callback, chatID int64, msgID int, v menuView) error {
	if v.User == nil {
		s.answerCb(cb.ID, "Сначала привяжите аккаунт.", true)
		return nil
	}
	last, err := repo.LastPwdResetAt(s.ctx(), s.DB, v.User.ID)
	if err != nil {
		s.answerCb(cb.ID, "Не получилось сменить пароль, попробуйте ещё раз.", true)
		return err
	}
	if last != nil {
		if left := pwdResetCooldown - time.Since(*last); left > 0 {
			s.answerCb(cb.ID, fmt.Sprintf("Пароль уже сбрасывали. Повторить можно через %d мин.", int(left.Minutes())+1), true)
			return nil
		}
	}

	pwd, err := randPassword14()
	if err != nil {
		return err
	}
	hash, err := bcrypt.GenerateFromPassword([]byte(pwd), 10)
	if err != nil {
		return err
	}
	if err := repo.SetPassword(s.ctx(), s.DB, v.User.ID, string(hash)); err != nil {
		s.answerCb(cb.ID, "Не получилось сменить пароль, попробуйте ещё раз.", true)
		return err
	}
	// Запись в журнале заявок остаётся — она же служит счётчиком КД и историей.
	if id, _, err := repo.CreatePwdReset(s.ctx(), s.DB, v.User.ID, chatID); err == nil {
		_, _ = repo.DecidePwdReset(s.ctx(), s.DB, id, models.PwdResetApproved, "авто")
	} else if s.Log != nil {
		s.Log.Warn("pwd-reset: журнал заявки", "err", err)
	}
	_ = repo.InsertAudit(s.ctx(), s.DB, nil, &v.User.ID, &v.User.ID, "pwd_reset_self", strPtr("hash_rotated"))
	s.notifyAdminsPwdReset(v.User)

	s.answerCb(cb.ID, "Пароль сброшен ✅", false)
	text := "🔑 <b>Новый пароль</b> (нажмите, чтобы показать):\n\n" +
		"<tg-spoiler><code>" + escHTML(pwd) + "</code></tg-spoiler>\n\n" +
		"<i>Войдите с ним в лаунчер и при желании смените в разделе «Пароль». " +
		"Старые сессии лаунчера при этом отзываются.</i>"
	return s.editMenuScreen(chatID, msgID, text, nil)
}

// notifyAdminsPwdReset — уведомление постфактум: решать нечего, кнопок нет.
func (s *Service) notifyAdminsPwdReset(u *models.User) {
	admins, err := repo.ListPrivilegedWithTelegram(s.ctx(), s.DB)
	if err != nil {
		if s.Log != nil {
			s.Log.Error("pwd-reset: список админов", "err", err)
		}
		return
	}
	tg := "—"
	if strings.TrimSpace(u.TelegramUsername) != "" {
		tg = "@" + escHTML(strings.TrimPrefix(strings.TrimSpace(u.TelegramUsername), "@"))
	}
	card := fmt.Sprintf(
		"🔑 <b>Самостоятельный сброс пароля</b>\n\n"+
			"👤 Игрок: <b>%s</b> (%s)\n"+
			"📧 Почта: %s\n\n"+
			"<i>Пароль выдан ботом автоматически, действий не требуется.</i>",
		escHTML(u.Login), tg, escHTML(maskEmailUnsafe(u.Email)))
	for _, a := range admins {
		if a.TelegramID == nil || !s.Cfg.AdminAllowlisted(*a.TelegramID) {
			continue
		}
		if err := s.notifyHTML(*a.TelegramID, card, nil); err != nil && s.Log != nil {
			s.Log.Warn("pwd-reset: уведомление админа", "tg", *a.TelegramID, "err", err)
		}
	}
	if s.Log != nil {
		s.Log.Info("pwd-reset: самостоятельный сброс", "login", u.Login)
	}
}
