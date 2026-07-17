package bot

import (
	"fmt"
	"strings"

	"launcher-backend/internal/models"
	"launcher-backend/internal/policy"
	"launcher-backend/internal/repo"
	"launcher-backend/internal/telegram"
)

// Callback-data экранов inline-меню. Формат "m:<screen>[:<action>]", ≤64 байт.
const (
	cbHome            = "m:home"
	cbProfile         = "m:profile"
	cbPwd             = "m:pwd"
	cbPwdChange       = "m:pwd:change"
	cbPwdReset        = "m:pwd:reset"
	cbEmail           = "m:email"
	cb2FA             = "m:2fa"
	cb2FAOn           = "m:2fa:on"
	cb2FAOff          = "m:2fa:off"
	cbDonate          = "m:donate"
	cbLauncher        = "m:launcher"
	cbLauncherFile    = "m:launcher:file"
	cbLauncherLinux   = "m:launcher:linux"
	cbLauncherWindows = "m:launcher:windows"
	cbLogin           = "m:login"
	cbRegister        = "m:register"
	cbAdmin           = "m:admin"
	cbPolicyAccept    = "m:policy:ok"
	cbPolicyRegAccept = "m:policy:reg"
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
	RulesURL        string
	HasLauncherFile bool
	// LauncherLinux / LauncherWindows — последний активный релиз под платформу
	// (nil — релиза нет). Когда хотя бы один есть, в разделе «Лаунчер»
	// показываются кнопки выбора платформы вместо старой «файл в чат».
	LauncherLinux   *launcherReleaseInfo
	LauncherWindows *launcherReleaseInfo
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
		rows := [][]telegram.InlineBtn{
			{{Text: "🔑 Войти", Data: cbLogin}, {Text: "📋 Регистрация", Data: cbRegister}},
			{{Text: "💎 Донат", Data: cbDonate}, {Text: "⬇ Лаунчер", Data: cbLauncher}},
		}
		rows = appendRulesRow(rows, v.RulesURL)
		return b.String(), telegram.InlineMarkup(rows...)
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
	rows = appendRulesRow(rows, v.RulesURL)
	if v.Admin {
		rows = append(rows, []telegram.InlineBtn{{Text: "🛠 Админка", Data: cbAdmin}})
	}
	return b.String(), telegram.InlineMarkup(rows...)
}

// appendRulesRow — URL-кнопка правил сервера. Без PUBLIC_BASE_URL кнопка
// выпадает (Telegram отклоняет сообщение с невалидным URL — BUTTON_URL_INVALID).
func appendRulesRow(rows [][]telegram.InlineBtn, rulesURL string) [][]telegram.InlineBtn {
	if strings.HasPrefix(rulesURL, "http://") || strings.HasPrefix(rulesURL, "https://") {
		rows = append(rows, []telegram.InlineBtn{{Text: "📜 Правила сервера", URL: rulesURL}})
	}
	return rows
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

// buildPasswordScreen — раздел «Пароль»: смена по старому паролю или заявка
// администратору («забыл пароль»).
func buildPasswordScreen(v menuView) (string, map[string]any) {
	text := "🔑 <b>Пароль</b>\n\n" +
		"🔄 <b>Сменить пароль</b> — если помните текущий: старый пароль → код → новый.\n\n" +
		"🆘 <b>Забыл пароль</b> — отправим заявку администратору; после одобрения " +
		"бот пришлёт новый пароль прямо в этот чат."
	return text, telegram.InlineMarkup(
		[]telegram.InlineBtn{{Text: "🔄 Сменить пароль", Data: cbPwdChange}},
		[]telegram.InlineBtn{{Text: "🆘 Забыл пароль", Data: cbPwdReset}},
		backRow(),
	)
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

// buildLauncherScreen — скачивание лаунчера. Если есть активные релизы —
// показывает кнопки выбора платформы (Linux / Windows), каждая качает последний
// релиз. Иначе откатывается на прямую ссылку и/или «файл в чат» (локальный exe).
func buildLauncherScreen(v menuView) (string, map[string]any) {
	hasRelease := v.LauncherLinux != nil || v.LauncherWindows != nil

	var verLine string
	if v.LauncherLinux != nil && v.LauncherWindows != nil && v.LauncherLinux.Version == v.LauncherWindows.Version {
		verLine = fmt.Sprintf("\nАктуальная версия: <b>v%s</b>", escHTML(v.LauncherLinux.Version))
	} else if v.LauncherLinux != nil && v.LauncherWindows != nil {
		verLine = fmt.Sprintf("\nLinux: <b>v%s</b> · Windows: <b>v%s</b>",
			escHTML(v.LauncherLinux.Version), escHTML(v.LauncherWindows.Version))
	} else if v.LauncherLinux != nil {
		verLine = fmt.Sprintf("\nАктуальная версия: <b>v%s</b>", escHTML(v.LauncherLinux.Version))
	} else if v.LauncherWindows != nil {
		verLine = fmt.Sprintf("\nАктуальная версия: <b>v%s</b>", escHTML(v.LauncherWindows.Version))
	}

	text := "⬇ <b>Лаунчер</b>\n\n" +
		"Выберите платформу — пришлём последний релиз прямо в чат." + verLine + "\n\n" +
		"<i>Вход в лаунчер — учётка сайта (привязка в этом боте).</i>"

	rows := [][]telegram.InlineBtn{}
	if hasRelease {
		var platRow []telegram.InlineBtn
		if v.LauncherLinux != nil {
			platRow = append(platRow, telegram.InlineBtn{Text: "🐧 Linux", Data: cbLauncherLinux})
		}
		if v.LauncherWindows != nil {
			platRow = append(platRow, telegram.InlineBtn{Text: "🪟 Windows", Data: cbLauncherWindows})
		}
		rows = append(rows, platRow)
	}
	if v.LauncherURL != "" {
		rows = append(rows, []telegram.InlineBtn{{Text: "🌐 Скачать с сайта", URL: v.LauncherURL}})
	}
	// Локальный файл в чат — только как фолбэк, когда релизов нет.
	if !hasRelease && v.HasLauncherFile {
		rows = append(rows, []telegram.InlineBtn{{Text: "📩 Прислать файл в чат", Data: cbLauncherFile}})
	}
	if !hasRelease && v.LauncherURL == "" && !v.HasLauncherFile {
		text = "⬇ <b>Лаунчер</b>\n\nСкачивание сейчас недоступно — загляните позже."
	}
	rows = append(rows, backRow())
	return text, telegram.InlineMarkup(rows...)
}

// buildPolicyScreen — блокирующий экран согласия для привязанного пользователя:
// пока политика не принята, все действия меню ведут сюда.
func buildPolicyScreen(privacyURL string) (string, map[string]any) {
	text := "🔒 <b>Политика конфиденциальности</b>\n\n" +
		"Мы обновили правила работы с данными. Чтобы продолжить пользоваться " +
		"ботом и играть на сервере, примите Политику конфиденциальности.\n\n" +
		"Мы собираем: логин, e-mail, Telegram ID, идентификатор оборудования (HWID), " +
		"IP-адрес, данные игровых сессий и античита, включая <b>скриншоты экрана " +
		"во время игры</b> (по запросу администрации).\n\n" +
		"<i>Полный текст — по кнопке ниже.</i>"
	// Без PUBLIC_BASE_URL кнопка чтения выпадает, но принятие работает
	// (иначе Telegram отклонит всё сообщение с невалидным URL BUTTON_URL_INVALID).
	var rows [][]telegram.InlineBtn
	if strings.HasPrefix(privacyURL, "http://") || strings.HasPrefix(privacyURL, "https://") {
		rows = append(rows, []telegram.InlineBtn{{Text: "📄 Читать полностью", URL: privacyURL}})
	}
	rows = append(rows, []telegram.InlineBtn{{Text: "✅ Принимаю", Data: cbPolicyAccept}})
	return text, telegram.InlineMarkup(rows...)
}

// buildRegPolicyScreen — шаг согласия перед регистрацией нового аккаунта.
// «Назад» ведёт на главный экран: без принятия регистрация не стартует.
func buildRegPolicyScreen(privacyURL string) (string, map[string]any) {
	text := "🔒 <b>Политика конфиденциальности</b>\n\n" +
		"Для создания аккаунта примите Политику конфиденциальности.\n\n" +
		"Мы собираем: логин, e-mail, Telegram ID, идентификатор оборудования (HWID), " +
		"IP-адрес, данные игровых сессий и античита, включая <b>скриншоты экрана " +
		"во время игры</b> (по запросу администрации).\n\n" +
		"<i>Полный текст — по кнопке ниже.</i>"
	// Graceful degradation: без валидного URL кнопка чтения не добавляется
	// (иначе Telegram отклонит сообщение с невалидным URL BUTTON_URL_INVALID).
	var rows [][]telegram.InlineBtn
	if strings.HasPrefix(privacyURL, "http://") || strings.HasPrefix(privacyURL, "https://") {
		rows = append(rows, []telegram.InlineBtn{{Text: "📄 Читать полностью", URL: privacyURL}})
	}
	rows = append(rows, []telegram.InlineBtn{{Text: "✅ Принимаю и продолжаю", Data: cbPolicyRegAccept}})
	rows = append(rows, backRow())
	return text, telegram.InlineMarkup(rows...)
}

// policyGateApplies — нужно ли вместо запрошенного экрана показать политику.
// Непривязанных (User == nil) не трогаем: их ограничивает callbackNeedsLink.
func policyGateApplies(v menuView, data string) bool {
	if v.User == nil || !policy.NeedsConsent(v.User) {
		return false
	}
	return data != cbPolicyAccept
}

// policyURL — публичная страница полного текста политики.
func (s *Service) policyURL() string {
	return strings.TrimRight(s.Cfg.PublicOrigin, "/") + "/privacy"
}

func (s *Service) rulesURL() string {
	return strings.TrimRight(s.Cfg.PublicOrigin, "/") + "/rules"
}

// policyGateText — гейт для текстовых сценариев привязанного пользователя:
// если согласия нет, шлёт экран политики новым меню-сообщением и возвращает true.
func (s *Service) policyGateText(chatID, telegramUID int64) (bool, error) {
	u, err := repo.FindUserByTelegram(s.ctx(), s.DB, telegramUID)
	if err != nil || u == nil {
		return false, err // непривязанных ловят существующие проверки linkedUID
	}
	if !policy.NeedsConsent(u) {
		return false, nil
	}
	text, markup := buildPolicyScreen(s.policyURL())
	if old, err2 := repo.ReadMenuMessage(s.ctx(), s.DB, chatID); err2 == nil && old > 0 {
		_ = telegram.DeleteMessage(s.HTTP, s.Cfg.TelegramBotToken, chatID, old)
	}
	id, err := telegram.SendMessageHTMLWithID(s.HTTP, s.Cfg.TelegramBotToken, chatID, text, markup, s.bannerPreview())
	if err != nil {
		return true, err
	}
	return true, repo.SaveMenuMessage(s.ctx(), s.DB, chatID, id)
}

// launcherCard показывает inline-экран «Лаунчер» новым меню-сообщением. Точка
// входа для команды /launcher и устаревшей reply-кнопки «Скачать лаунчер»
// (вместо прежней прямой отправки локального .exe, который на проде мог быть
// недоступен). Дальше игрок выбирает платформу кнопками экрана.
func (s *Service) launcherCard(chatID int64, telegramUID int64) error {
	v, err := s.menuViewFor(telegramUID)
	if err != nil {
		return err
	}
	text, markup := buildLauncherScreen(v)
	if old, err2 := repo.ReadMenuMessage(s.ctx(), s.DB, chatID); err2 == nil && old > 0 {
		_ = telegram.DeleteMessage(s.HTTP, s.Cfg.TelegramBotToken, chatID, old)
	}
	id, err := telegram.SendMessageHTMLWithID(s.HTTP, s.Cfg.TelegramBotToken, chatID, text, markup, s.bannerPreview())
	if err != nil {
		return err
	}
	return repo.SaveMenuMessage(s.ctx(), s.DB, chatID, id)
}

// clearLegacyKeyboard снимает устаревшую нижнюю reply-клавиатуру: шлёт служебное
// сообщение с remove_keyboard и тут же удаляет его. Нужно на входе в меню, т.к.
// inline-меню reply-клавиатуру не сбрасывает, а у части игроков осталась старая
// клавиатура на пол-экрана (до редизайна).
func (s *Service) clearLegacyKeyboard(chatID int64) {
	id, err := telegram.SendMessageHTMLWithID(
		s.HTTP, s.Cfg.TelegramBotToken, chatID, "🏠",
		telegram.ReplyKeyboardRemove(), telegram.LinkPreviewBanner(""))
	if err != nil {
		if s.Log != nil {
			s.Log.Warn("clear legacy keyboard", "err", err)
		}
		return
	}
	if id > 0 {
		_ = telegram.DeleteMessage(s.HTTP, s.Cfg.TelegramBotToken, chatID, id)
	}
}

// keyboardDismiss — reply_markup, снимающий нижнюю reply-клавиатуру. Навигация
// живёт в inline-меню и команде /menu (синяя кнопка «☰» Telegram), поэтому
// постоянную клавиатуру на пол-экрана мы не показываем, а у кого осталась
// старая (до редизайна) — снимаем этим markup при любом ответе.
func keyboardDismiss() map[string]any {
	return telegram.ReplyKeyboardRemove()
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
		RulesURL:        s.rulesURL(),
		HasLauncherFile: s.launcherExePath() != "",
		LauncherLinux:   s.latestLauncherInfo("linux-x64"),
		LauncherWindows: s.latestLauncherInfo("windows-x64"),
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
