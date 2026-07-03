package bot

import (
	"fmt"
	"strconv"
	"strings"

	"launcher-backend/internal/models"
	"launcher-backend/internal/repo"
	"launcher-backend/internal/telegram"
	"launcher-backend/internal/validators"

	"golang.org/x/crypto/bcrypt"
)

// roleRank — числовой ранг роли для сравнения привилегий (больше = привилегированнее).
// Используется, чтобы запретить эскалацию moderator → admin через админ-действия бота.
func roleRank(role string) int {
	switch strings.ToLower(strings.TrimSpace(role)) {
	case models.RoleAdmin:
		return 2
	case models.RoleModerator:
		return 1
	default:
		return 0
	}
}

// ensureCanManageTarget проверяет, что бот-админ (по telegramUID) вправе выполнять
// деструктивное действие над target. Запрещено трогать цель, чья роль НЕ НИЖЕ роли
// исполнителя: модератор не тронет админа/модератора, админ не тронет другого админа
// (и себя как админа). Возвращает allowed=false, если действие отклонено (уведомление
// пользователю уже отправлено).
func (s *Service) ensureCanManageTarget(chatID int64, telegramUID int64, targetID string) (bool, error) {
	adm, err := s.resolveAdmin(telegramUID)
	if err != nil {
		return false, err
	}
	if adm == nil {
		return false, s.notifyWarn(chatID, "Действие недоступно: нет прав администратора.")
	}
	tgt, err := repo.FindUserByID(s.ctx(), s.DB, targetID)
	if err != nil {
		return false, err
	}
	if tgt == nil {
		return false, s.notifyWarn(chatID, "Цель не найдена — повторите поиск.")
	}
	if roleRank(tgt.Role) >= roleRank(adm.user.Role) {
		return false, s.notifyWarn(chatID, fmt.Sprintf(
			"Недостаточно прав: нельзя выполнять действия над аккаунтом с ролью «%s» — она не ниже вашей («%s»).",
			tgt.Role, adm.user.Role))
	}
	return true, nil
}

func (s *Service) adminOpsKeyboardMarkup() map[string]any {
	k := &telegram.ReplyKeyboardStyled{
		Rows: [][]telegram.KeyboardBtn{
			{{Text: "🔍 Поиск", Style: "primary"}, {Text: "📡 OPS", Style: "success"}},
			{{Text: "⬅ Выйти из админки", Style: "danger"}},
		},
		Resize:           true,
		InputPlaceholder: "Поиск пользователей или команды",
	}
	return k.ToReplyMarkup()
}

func (s *Service) adminUserKeyboardMarkup() map[string]any {
	k := &telegram.ReplyKeyboardStyled{
		Rows: [][]telegram.KeyboardBtn{
			{{Text: "📧 Изменить email цели", Style: "primary"}, {Text: "🔓 Сгенерировать пароль", Style: "danger"}},
			{{Text: "ℹ Инфо пользователя", Style: "success"}, {Text: "⬅ К списку поиска", Style: "primary"}},
		},
		Resize: true,
	}
	return k.ToReplyMarkup()
}

func (s *Service) adminMenuActions(chatID int64, telegramUID int64, text string, _ *adminContext) error {
	switch text {
	case "🔍 Поиск":
		ep := repo.EmptyPayload()
		if err := repo.SaveDialogue(s.ctx(), s.DB, chatID, repo.FlowAdminSearch, &ep); err != nil {
			return err
		}
		return s.notifyHTML(chatID,
			s.msgWithCancelHint(
				"Напишите, кого ищем — одним сообщением:\n"+
					"• часть <b>игрового ника</b>;\n"+
					"• или логин / фрагмент почты;\n"+
					"• или <b>uuid</b> пользователя.\n\n"+
					"Бот покажет до 10 совпадений — ответьте номером строки."),
			homeReplyKeyboardMarkup())

	case "📡 OPS":
		d := opsFormatDigest(s.HTTP, s.Cfg)
		return s.notifyHTML(chatID, "<pre>"+escHTML(d)+"</pre>", s.adminOpsKeyboardMarkup())

	case "⬅ Выйти из админки":
		if err := repo.ClearDialogue(s.ctx(), s.DB, chatID); err != nil {
			return err
		}
		return s.sendHomeMenu(chatID, telegramUID, "Вы вышли из панели администратора.")

	default:
		return s.notifyWarn(chatID, "Выберите кнопку на админ-клавиатуре («Поиск», «OPS», «Выйти») или /cancel.")
	}
}

func (s *Service) adminSearch(chatID int64, query string, adminOpt *adminContext) error {
	if adminOpt == nil {
		return fmt.Errorf("нет прав")
	}
	q := strings.TrimSpace(query)
	if q == "" {
		return s.notifyWarn(chatID, "Введите непустой запрос: ник, логин, почту или uuid. Или вернитесь кнопкой «⬅ Выйти из админки».")
	}
	rows, err := repo.SearchUsers(s.ctx(), s.DB, q)
	if err != nil {
		return err
	}
	if len(rows) == 0 {
		_ = s.notifyWarn(chatID, "Никого не нашли по этому запросу. Попробуйте короче фрагмент ника или другой вариант (логин, почта).")
		ep := repo.EmptyPayload()
		_ = repo.SaveDialogue(s.ctx(), s.DB, chatID, repo.FlowAdminMenu, &ep)
		return s.notifyHTML(chatID, "Выберите раздел ниже.", s.adminOpsKeyboardMarkup())
	}

	var ids []string
	var lines []string
	for i, row := range rows {
		ids = append(ids, row.ID)
		var extra string
		if row.TelegramID != nil {
			extra = fmt.Sprintf(" · tg <code>%d</code>", *row.TelegramID)
		}
		lines = append(lines, fmt.Sprintf("%d · <code>%s</code> %s%s",
			i+1, escHTML(row.Login), escHTML(maskEmailUnsafe(row.Email)), extra))
	}
	dp := repo.DialoguePayload{AdminPickIDs: ids}
	if err := repo.SaveDialogue(s.ctx(), s.DB, chatID, repo.FlowAdminAwaitPick, &dp); err != nil {
		return err
	}
	body := s.msgWithCancelHint(fmt.Sprintf(
		"Найдено строк: <b>%d</b>. Ответьте <b>только числом</b> (номер строки 1…%d), без текста.\n\n%s",
		len(lines), len(lines), strings.Join(lines, "\n")))
	return s.notifyHTML(chatID, body, s.adminOpsKeyboardMarkup())
}

func (s *Service) adminPick(chatID int64, text string, payload repo.DialoguePayload, adminOpt *adminContext) error {
	if adminOpt == nil {
		return fmt.Errorf("нет прав")
	}
	pick := payload.AdminPickIDs
	if len(pick) == 0 {
		return fmt.Errorf("список пропал")
	}
	idxStr := strings.TrimSpace(text)
	var idx int
	if ui, err := strconv.ParseUint(idxStr, 10, 64); err != nil || ui == 0 {
		idx = 0
	} else {
		idx = int(ui) - 1
	}
	if idx < 0 || idx >= len(pick) {
		_ = s.notifyWarn(chatID, "Такого номера нет. Повторите поиск или /cancel.")
		ep := repo.EmptyPayload()
		_ = repo.SaveDialogue(s.ctx(), s.DB, chatID, repo.FlowAdminMenu, &ep)
		return s.notifyHTML(chatID, "Выберите раздел ниже.", s.adminOpsKeyboardMarkup())
	}

	targetID := pick[idx]
	dp := repo.DialoguePayload{}
	dp.AdminTargetID = &targetID
	if err := repo.SaveDialogue(s.ctx(), s.DB, chatID, repo.FlowAdminManaging, &dp); err != nil {
		return err
	}
	card, err := s.fetchUserSummary(targetID)
	if err != nil {
		return err
	}
	return s.notifyHTML(chatID, card, s.adminUserKeyboardMarkup())
}

func (s *Service) fetchUserSummary(userID string) (string, error) {
	u, err := repo.FindUserByID(s.ctx(), s.DB, userID)
	if err != nil {
		return "", err
	}
	if u == nil {
		return "", fmt.Errorf("пользователя нет")
	}
	tgID := "—"
	tgUsername := "—"
	if u.TelegramID != nil {
		tgID = fmt.Sprintf("<code>%d</code>", *u.TelegramID)
	}
	if strings.TrimSpace(u.TelegramUsername) != "" {
		tgUsername = "@" + escHTML(strings.TrimPrefix(strings.TrimSpace(u.TelegramUsername), "@"))
	}
	return fmt.Sprintf(
		"<b>%s</b> / %s\n"+
			"<b>UUID аккаунта</b>: <code>%s</code>\n"+
			"<b>Роль</b>: %s\n"+
			"<b>Telegram ID</b>: %s\n"+
			"<b>TG username</b>: %s\n"+
			"<b>Создан</b>: %s",
		escHTML(u.Login), escHTML(maskEmailUnsafe(u.Email)),
		escHTML(u.ProviderUUID),
		escHTML(u.Role),
		tgID, tgUsername,
		escHTML(u.CreatedAt.UTC().Format("2006-01-02 15:04:05")),
	), nil
}

func (s *Service) adminManage(chatID int64, telegramUIDFrom int64, text string) error {
	_, pl, err := repo.ReadDialogue(s.ctx(), s.DB, chatID)
	if err != nil {
		return err
	}
	if pl.AdminTargetID == nil {
		return fmt.Errorf("не выбран пользователь")
	}
	target := *pl.AdminTargetID
	admT := telegramUIDFrom

	switch text {
	case "📧 Изменить email цели":
		if ok, err := s.ensureCanManageTarget(chatID, admT, target); err != nil {
			return err
		} else if !ok {
			return nil
		}
		if err := s.notifyHTML(chatID, s.msgWithCancelHint("Введите новый <b>e-mail</b> аккаунта."), homeReplyKeyboardMarkup()); err != nil {
			return err
		}
		return repo.SaveDialogue(s.ctx(), s.DB, chatID, repo.FlowAdminAskNewEmail, &pl)

	case "🔓 Сгенерировать пароль":
		if ok, err := s.ensureCanManageTarget(chatID, admT, target); err != nil {
			return err
		} else if !ok {
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
		if err := repo.SetPassword(s.ctx(), s.DB, target, string(hash)); err != nil {
			return err
		}
		td := admT
		_ = repo.InsertAudit(s.ctx(), s.DB, &td, nil, &target, "admin_generate_password", strPtr("hash_rotated"))
		msg := fmt.Sprintf("Временный пароль отправлен здесь же (передайте игроку вручную): <code>%s</code>", escHTML(pwd))
		return s.notifyHTML(chatID, msg, s.adminUserKeyboardMarkup())

	case "ℹ Инфо пользователя":
		card, err := s.fetchUserSummary(target)
		if err != nil {
			return err
		}
		return s.notifyHTML(chatID, card, s.adminUserKeyboardMarkup())

	case "⬅ К списку поиска":
		ep := repo.EmptyPayload()
		if err := repo.SaveDialogue(s.ctx(), s.DB, chatID, repo.FlowAdminSearch, &ep); err != nil {
			return err
		}
		return s.notifyHTML(chatID, s.msgWithCancelHint("Отправьте новый текст поиска."), homeReplyKeyboardMarkup())

	default:
		return s.notifyWarn(chatID, "Выберите кнопку из списка.")
	}
}

func (s *Service) adminApplyEmail(chatID int64, adminTID int64, mail string) error {
	_, pl, err := repo.ReadDialogue(s.ctx(), s.DB, chatID)
	if err != nil {
		return err
	}
	if pl.AdminTargetID == nil {
		return fmt.Errorf("цель потеряна")
	}
	target := *pl.AdminTargetID
	if ok, err := s.ensureCanManageTarget(chatID, adminTID, target); err != nil {
		return err
	} else if !ok {
		return nil
	}
	if !validators.IsValidEmail(true, mail) {
		return s.notifyWarn(chatID, "Некорректный email.")
	}

	if err := repo.SetEmail(s.ctx(), s.DB, target, strings.TrimSpace(mail)); err != nil {
		return err
	}
	d := strings.TrimSpace(mail)
	_ = repo.InsertAudit(s.ctx(), s.DB, &adminTID, nil, &target, "admin_email", &d)

	body := fmt.Sprintf("Почта пользователя изменена администратором на %s", escHTML(mail))
	if err := s.notifyHTML(chatID, body, s.adminUserKeyboardMarkup()); err != nil {
		return err
	}
	return repo.SaveDialogue(s.ctx(), s.DB, chatID, repo.FlowAdminManaging, &pl)
}
