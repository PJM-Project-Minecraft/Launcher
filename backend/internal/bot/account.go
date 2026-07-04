package bot

import (
	"fmt"
	"strings"

	"launcher-backend/internal/repo"
	"launcher-backend/internal/validators"

	"golang.org/x/crypto/bcrypt"
)

func (s *Service) handlePasswordOld(chatID int64, messageID int, telegramUID int64, text string, payload repo.DialoguePayload) error {
	uidPtr, err := s.linkedUID(telegramUID)
	if err != nil {
		return err
	}
	if uidPtr == nil {
		if err := s.forbidNotLinked(chatID, telegramUID); err != nil {
			return err
		}
		return repo.ClearDialogue(s.ctx(), s.DB, chatID)
	}
	uid := *uidPtr
	u, err := repo.FindUserByID(s.ctx(), s.DB, uid)
	if err != nil {
		return err
	}
	if u == nil {
		return fmt.Errorf("нет пользователя")
	}
	pw := strings.TrimSpace(text)
	if pw != "" && messageID > 0 {
		s.redactPasswordMessage(chatID, messageID, pw)
	}
	if bcrypt.CompareHashAndPassword([]byte(u.PasswordHash), []byte(pw)) != nil {
		_ = s.notifyWarn(chatID, "Текущий пароль не совпал. Попробуйте ещё раз или /cancel.")
		ep := repo.EmptyPayload()
		return repo.SaveDialogue(s.ctx(), s.DB, chatID, repo.FlowChangePwdOld, &ep)
	}

	code, err := otpPlain()
	if err != nil {
		return err
	}
	if err := s.persistOTP(uid, chatID, purposePwd, code); err != nil {
		return err
	}
	_ = s.notifyHTML(chatID, s.msgWithCancelHint(fmt.Sprintf(
		"На смену пароля отправлен <b>одноразовый код</b> (около %d мин.):\n<code>%s</code>\n\n"+
			"Введите эти <b>6 цифр</b> следующим сообщением.",
		otpMinutes, escHTML(code))), keyboardDismiss())

	dp := payload
	dp.OtpUserID = &uid
	return repo.SaveDialogue(s.ctx(), s.DB, chatID, repo.FlowChangePwdWaitOtp, &dp)
}

func (s *Service) handlePasswordAfterOTP(chatID int64, _telegramUID int64, text string, payload repo.DialoguePayload) error {
	if payload.OtpUserID == nil {
		return fmt.Errorf("неизвестная сессия")
	}
	uid := *payload.OtpUserID
	ok, err := s.consumeOTPCheck(chatID, uid, purposePwd, strings.TrimSpace(text))
	if err != nil {
		return err
	}
	if !ok {
		_ = s.notifyWarn(chatID, "Код для смены пароля неверный или истёк. Начните заново: /cancel → «Сменить пароль».")
		return repo.ClearDialogue(s.ctx(), s.DB, chatID)
	}
	_ = s.notifyHTML(chatID, s.msgWithCancelHint(
		"Код принят. Теперь пришлите <b>новый пароль</b> одним сообщением (8–128 символов).\n\n"+
			"<i>Сообщение с паролем будет удалено из чата.</i>"),
		keyboardDismiss())

	dp := repo.DialoguePayload{}
	dp.OtpUserID = &uid
	return repo.SaveDialogue(s.ctx(), s.DB, chatID, repo.FlowChangePwdNew, &dp)
}

func (s *Service) handlePasswordNew(chatID int64, messageID int, telegramUID int64, text string, payload repo.DialoguePayload) error {
	if payload.OtpUserID == nil {
		return fmt.Errorf("нет user id")
	}
	uid := *payload.OtpUserID
	pw := strings.TrimSpace(text)
	if pw != "" && messageID > 0 {
		s.redactPasswordMessage(chatID, messageID, pw)
	}
	if !validators.IsValidPassword(pw) {
		return s.notifyWarn(chatID, "Новый пароль должен быть от 8 до 128 символов — попробуйте другой.")
	}
	hash, err := bcrypt.GenerateFromPassword([]byte(pw), 10)
	if err != nil {
		return err
	}
	if err := repo.SetPassword(s.ctx(), s.DB, uid, string(hash)); err != nil {
		return err
	}
	_ = repo.ClearDialogue(s.ctx(), s.DB, chatID)
	return s.sendHomeMenu(chatID, telegramUID, "✅ <b>Пароль обновлён.</b> Используйте его в лаунчере и при входе.")
}

func (s *Service) handleChangeEmailAsk(chatID int64, telegramUID int64, payload repo.DialoguePayload, text string) (repo.DialoguePayload, error) {
	uidPtr, err := s.linkedUID(telegramUID)
	if err != nil {
		return payload, err
	}
	if uidPtr == nil {
		if err := s.forbidNotLinked(chatID, telegramUID); err != nil {
			return payload, err
		}
		_ = repo.ClearDialogue(s.ctx(), s.DB, chatID)
		return repo.EmptyPayload(), nil
	}
	uid := *uidPtr

	newMail := strings.TrimSpace(text)
	if !validators.IsValidEmail(true, newMail) {
		_ = s.notifyWarn(chatID, "Проверьте формат почты: латиница, @, домен. Пример: <code>name@mail.com</code>")
		return payload, nil
	}

	u, err := repo.FindUserByID(s.ctx(), s.DB, uid)
	if err != nil {
		return payload, err
	}
	if u == nil {
		return payload, fmt.Errorf("нет пользователя")
	}

	code, err := otpPlain()
	if err != nil {
		return payload, err
	}
	dp := payload
	dp.PendingNewEmail = &newMail
	if err := s.persistOTP(uid, chatID, purposeEmail, code); err != nil {
		return payload, err
	}
	_ = s.notifyHTML(chatID, s.msgWithCancelHint(fmt.Sprintf(
		"Чтобы подтвердить новую почту, введите код из сообщения ниже (около %d мин.):\n<code>%s</code>\n\n"+
			"<i>Код только для этого чата — пересылать другим не нужно.</i>",
		otpMinutes, escHTML(code))), keyboardDismiss())

	ui := u.ID
	dp.OtpUserID = &ui
	if err := repo.SaveDialogue(s.ctx(), s.DB, chatID, repo.FlowChangeEmailWaitOtp, &dp); err != nil {
		return payload, err
	}
	return dp, nil
}

func (s *Service) handleChangeEmailOTP(chatID int64, telegramUID int64, text string, payload repo.DialoguePayload) error {
	if payload.OtpUserID == nil {
		return fmt.Errorf("нет uid")
	}
	uid := *payload.OtpUserID
	ok, err := s.consumeOTPCheck(chatID, uid, purposeEmail, strings.TrimSpace(text))
	if err != nil {
		return err
	}
	if !ok {
		_ = s.notifyWarn(chatID, "Код для смены почты неверный или истёк. Начните заново: /cancel → «Email».")
		return repo.ClearDialogue(s.ctx(), s.DB, chatID)
	}
	newMail := ""
	if payload.PendingNewEmail != nil {
		newMail = *payload.PendingNewEmail
	}
	if newMail == "" {
		return fmt.Errorf("не было нового email")
	}
	if err := repo.SetEmail(s.ctx(), s.DB, uid, newMail); err != nil {
		return err
	}
	_ = repo.ClearDialogue(s.ctx(), s.DB, chatID)
	return s.sendHomeMenu(chatID, telegramUID, fmt.Sprintf("✅ <b>Почта обновлена:</b> <code>%s</code>", escHTML(newMail)))
}
