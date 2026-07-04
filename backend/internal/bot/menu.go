package bot

import (
	"fmt"
	"strings"

	"launcher-backend/internal/models"
	"launcher-backend/internal/repo"
	"launcher-backend/internal/telegram"
)

// Callback-data экранов inline-меню. Формат "m:<screen>[:<action>]", ≤64 байт.
const (
	cbHome         = "m:home"
	cbProfile      = "m:profile"
	cbPwd          = "m:pwd"
	cbEmail        = "m:email"
	cb2FA          = "m:2fa"
	cb2FAOn        = "m:2fa:on"
	cb2FAOff       = "m:2fa:off"
	cbDonate       = "m:donate"
	cbLauncher     = "m:launcher"
	cbLauncherFile = "m:launcher:file"
	cbLogin        = "m:login"
	cbRegister     = "m:register"
	cbAdmin        = "m:admin"
)

const menuButtonLabel = "🏠 Меню"

// menuView — данные для рендера экранов меню. User == nil — аккаунт не привязан.
type menuView struct {
	User            *models.User
	Admin           bool
	Brand           string
	Tagline         string
	DonateURL       string
	LauncherURL     string
	HasLauncherFile bool
}

func backRow() []telegram.InlineBtn {
	return []telegram.InlineBtn{{Text: "← Назад", Data: cbHome}}
}

// roleDisplay — человекочитаемая роль для шапки меню; неизвестные значения как есть.
func roleDisplay(role string) string {
	switch strings.ToLower(strings.TrimSpace(role)) {
	case "admin":
		return "админ"
	case "moderator":
		return "модератор"
	case "user":
		return "игрок"
	}
	return role
}

const menuDivider = "──────────────────"

// buildHomeScreen — главный экран. notice выводится первой строкой
// (результат завершённого сценария: «Пароль обновлён» и т.п.).
func buildHomeScreen(v menuView, notice string) (string, map[string]any) {
	var b strings.Builder
	if notice != "" {
		b.WriteString(notice + "\n\n")
	}
	b.WriteString("⛏ <b>" + escHTML(v.Brand) + "</b>\n")
	if v.Tagline != "" {
		b.WriteString("<i>" + escHTML(v.Tagline) + "</i>\n")
	}
	b.WriteString(menuDivider + "\n\n")

	if v.User == nil {
		b.WriteString("Добро пожаловать! 👋\n" +
			"Привяжи аккаунт — и управляй паролем, почтой и 2FA прямо здесь:\n\n" +
			"🔑 <b>Войти</b> — учётка уже есть\n" +
			"📋 <b>Регистрация</b> — создать новую\n\n" +
			"<i>Всё управление — кнопками ниже 👇</i>")
		return b.String(), telegram.InlineMarkup(
			[]telegram.InlineBtn{{Text: "🔑 Войти", Data: cbLogin}, {Text: "📋 Регистрация", Data: cbRegister}},
			[]telegram.InlineBtn{{Text: "💎 Донат", Data: cbDonate}, {Text: "⬇ Лаунчер", Data: cbLauncher}},
		)
	}

	totp := "❌ выключена"
	if v.User.TOTPEnabled {
		totp = "✅ включена"
	}
	b.WriteString(fmt.Sprintf("👋 Привет, <b>%s</b>!\n\n", escHTML(v.User.Login)))
	b.WriteString(fmt.Sprintf("🛡 2FA · %s\n", totp))
	b.WriteString(fmt.Sprintf("⭐ Роль · %s\n\n", escHTML(roleDisplay(v.User.Role))))
	b.WriteString("<i>Выбери раздел кнопками ниже 👇</i>")

	rows := [][]telegram.InlineBtn{
		{{Text: "👤 Профиль", Data: cbProfile}, {Text: "🔑 Пароль", Data: cbPwd}},
		{{Text: "📧 Email", Data: cbEmail}, {Text: "🛡 2FA", Data: cb2FA}},
		{{Text: "💎 Донат", Data: cbDonate}, {Text: "⬇ Лаунчер", Data: cbLauncher}},
	}
	if v.Admin {
		rows = append(rows, []telegram.InlineBtn{{Text: "🛠 Админка", Data: cbAdmin}})
	}
	return b.String(), telegram.InlineMarkup(rows...)
}

// buildProfileScreen — карточка профиля (только для привязанных; guard в диспетчере).
func buildProfileScreen(v menuView) (string, map[string]any) {
	u := v.User
	totpLine := "выключена — включите в разделе «2FA»"
	if u.TOTPEnabled {
		totpLine = "включена ✅"
	}
	text := strings.Join([]string{
		"👤 <b>Профиль</b>",
		"",
		fmt.Sprintf("<b>UUID</b>: <code>%s</code>", escHTML(u.ProviderUUID)),
		fmt.Sprintf("<b>Логин</b>: %s", escHTML(u.Login)),
		fmt.Sprintf("<b>Почта</b>: %s", escHTML(maskEmailUnsafe(u.Email))),
		fmt.Sprintf("<b>Роль</b>: %s", escHTML(u.Role)),
		fmt.Sprintf("<b>Регистрация</b>: %s", escHTML(u.CreatedAt.UTC().Format("2006-01-02 15:04:05"))),
		fmt.Sprintf("<b>2FA лаунчера</b>: %s", totpLine),
	}, "\n")
	return text, telegram.InlineMarkup(backRow())
}

// build2FAScreen — статус 2FA и контекстная кнопка включения/выключения.
func build2FAScreen(v menuView) (string, map[string]any) {
	u := v.User
	if u.TOTPEnabled {
		text := "🛡 <b>Двухфакторная защита лаунчера</b>\n\n" +
			"Статус: <b>включена</b> ✅\n" +
			"При входе в лаунчер после пароля запрашивается код из приложения.\n" +
			"Потеряли телефон — восстановление только через администратора.\n\n" +
			"<i>Отключение попросит пароль и текущий код — защита от случайного сброса.</i>"
		return text, telegram.InlineMarkup(
			[]telegram.InlineBtn{{Text: "Выключить 2FA", Data: cb2FAOff}},
			backRow(),
		)
	}
	text := "🛡 <b>Двухфакторная защита лаунчера</b>\n\n" +
		"Статус: <b>выключена</b> ❌\n" +
		"Включите — и вход в лаунчер потребует код из приложения-аутентификатора.\n\n" +
		"<i>Понадобится Google Authenticator, Authy или аналог.</i>"
	return text, telegram.InlineMarkup(
		[]telegram.InlineBtn{{Text: "Включить 2FA", Data: cb2FAOn}},
		backRow(),
	)
}

// buildDonateScreen — витрина: URL-кнопка прямо в магазин.
func buildDonateScreen(v menuView) (string, map[string]any) {
	text := "💎 <b>Донат и магазин</b>\n\n" +
		"Покупки и оплата проходят на сайте магазина — кнопка ниже.\n\n" +
		"<i>Спасибо за поддержку проекта!</i>"
	rows := [][]telegram.InlineBtn{}
	if v.DonateURL != "" {
		rows = append(rows, []telegram.InlineBtn{{Text: "🛒 Открыть магазин", URL: v.DonateURL}})
	}
	rows = append(rows, backRow())
	return text, telegram.InlineMarkup(rows...)
}

// buildLauncherScreen — скачивание лаунчера: прямая ссылка и/или файл в чат.
func buildLauncherScreen(v menuView) (string, map[string]any) {
	text := "⬇ <b>Лаунчер</b>\n\n" +
		"Скачайте по прямой ссылке или получите файл прямо в чат.\n\n" +
		"<i>Вход в лаунчер — учётка сайта (привязка в этом боте).</i>"
	rows := [][]telegram.InlineBtn{}
	if v.LauncherURL != "" {
		rows = append(rows, []telegram.InlineBtn{{Text: "🌐 Скачать с сайта", URL: v.LauncherURL}})
	}
	if v.HasLauncherFile {
		rows = append(rows, []telegram.InlineBtn{{Text: "📩 Прислать файл в чат", Data: cbLauncherFile}})
	}
	if v.LauncherURL == "" && !v.HasLauncherFile {
		text = "⬇ <b>Лаунчер</b>\n\nРаздача лаунчера сейчас не настроена. Обратитесь к администратору проекта."
	}
	rows = append(rows, backRow())
	return text, telegram.InlineMarkup(rows...)
}

// homeReplyKeyboardMarkup — постоянная reply-клавиатура из одной кнопки «🏠 Меню».
func homeReplyKeyboardMarkup() map[string]any {
	k := &telegram.ReplyKeyboardStyled{
		Rows:             [][]telegram.KeyboardBtn{{{Text: menuButtonLabel, Style: "primary"}}},
		Resize:           true,
		InputPlaceholder: "«🏠 Меню» — вернуться на главный экран",
	}
	return k.ToReplyMarkup()
}

// menuViewFor собирает данные экрана для пользователя Telegram.
func (s *Service) menuViewFor(telegramUID int64) (menuView, error) {
	u, err := repo.FindUserByTelegram(s.ctx(), s.DB, telegramUID)
	if err != nil {
		return menuView{}, err
	}
	adm, err := s.resolveAdmin(telegramUID)
	if err != nil {
		return menuView{}, err
	}
	return menuView{
		User:            u,
		Admin:           adm != nil,
		Brand:           s.Cfg.BrandPublicName,
		Tagline:         s.Cfg.BrandTagline,
		DonateURL:       s.Cfg.DonateShopURL,
		LauncherURL:     s.Cfg.LauncherDirectDownloadURL(),
		HasLauncherFile: s.launcherExePath() != "",
	}, nil
}

// bannerPreview — link_preview_options меню (nil нельзя: пустой URL отключает превью).
func (s *Service) bannerPreview() map[string]any {
	return telegram.LinkPreviewBanner(strings.TrimSpace(s.Cfg.BotBannerURL))
}

// sendHomeMenu шлёт свежее меню-сообщение (главный экран) внизу чата,
// удаляет предыдущее меню и запоминает новый message_id.
func (s *Service) sendHomeMenu(chatID, telegramUID int64, notice string) error {
	v, err := s.menuViewFor(telegramUID)
	if err != nil {
		return err
	}
	text, markup := buildHomeScreen(v, notice)
	if old, err := repo.ReadMenuMessage(s.ctx(), s.DB, chatID); err == nil && old > 0 {
		// Старое меню убираем, чтобы в истории не жило два «живых» меню.
		_ = telegram.DeleteMessage(s.HTTP, s.Cfg.TelegramBotToken, chatID, old)
	}
	id, err := telegram.SendMessageHTMLWithID(s.HTTP, s.Cfg.TelegramBotToken, chatID, text, markup, s.bannerPreview())
	if err != nil {
		return err
	}
	return repo.SaveMenuMessage(s.ctx(), s.DB, chatID, id)
}

// editMenuScreen редактирует меню на месте; если сообщение протухло
// (удалено/слишком старое) — шлёт новое и обновляет сохранённый id.
func (s *Service) editMenuScreen(chatID int64, messageID int, text string, markup map[string]any) error {
	err := telegram.EditMessageTextHTML(s.HTTP, s.Cfg.TelegramBotToken, chatID, messageID, text, markup, s.bannerPreview())
	if err == nil {
		return repo.SaveMenuMessage(s.ctx(), s.DB, chatID, messageID)
	}
	if strings.Contains(err.Error(), "message is not modified") {
		return nil // повторное нажатие того же экрана — не ошибка
	}
	if s.Log != nil {
		s.Log.Warn("edit меню, пересоздаю", "err", err)
	}
	id, sendErr := telegram.SendMessageHTMLWithID(s.HTTP, s.Cfg.TelegramBotToken, chatID, text, markup, s.bannerPreview())
	if sendErr != nil {
		return sendErr
	}
	return repo.SaveMenuMessage(s.ctx(), s.DB, chatID, id)
}
