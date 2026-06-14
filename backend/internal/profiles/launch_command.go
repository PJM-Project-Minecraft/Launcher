package profiles

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"launcher-backend/internal/models"
)

// launchTarget описывает одну ОС, под которую собирается команда запуска.
type launchTarget struct {
	osName       string // имя ОС в нотации Mojang: windows / linux / osx
	separator    string // разделитель classpath: ; для windows, иначе :
	launcherName string
}

var launchTargets = map[string]launchTarget{
	"windows": {osName: "windows", separator: ";", launcherName: "ProjectMinecraftLauncher"},
	"linux":   {osName: "linux", separator: ":", launcherName: "ProjectMinecraftLauncher"},
	"macos":   {osName: "osx", separator: ":", launcherName: "ProjectMinecraftLauncher"},
}

// versionJSON — разбор формата version manifest Minecraft (как ваниль, так и
// профили загрузчиков вроде NeoForge/Fabric с полем inheritsFrom).
type versionJSON struct {
	ID           string          `json:"id"`
	Type         string          `json:"type"`
	InheritsFrom string          `json:"inheritsFrom"`
	MainClass    string          `json:"mainClass"`
	AssetIndex   *downloadInfo   `json:"assetIndex"`
	Assets       string          `json:"assets"`
	Libraries    []versionLib    `json:"libraries"`
	Arguments    *versionArgs    `json:"arguments"`
	MinecraftArg string          `json:"minecraftArguments"`
	Downloads    json.RawMessage `json:"downloads"`
}

type versionArgs struct {
	Game []json.RawMessage `json:"game"`
	JVM  []json.RawMessage `json:"jvm"`
}

type versionLib struct {
	Name      string `json:"name"`
	URL       string `json:"url"`
	Downloads *struct {
		Artifact *artifactInfo `json:"artifact"`
	} `json:"downloads"`
	Rules []versionRule `json:"rules"`
}

type versionRule struct {
	Action   string                 `json:"action"`
	OS       *versionRuleOS         `json:"os"`
	Features map[string]bool        `json:"features"`
	Extra    map[string]interface{} `json:"-"`
}

type versionRuleOS struct {
	Name string `json:"name"`
	Arch string `json:"arch"`
}

// argToken — элемент списка аргументов: либо строка, либо объект {rules, value}.
type argToken struct {
	rules  []versionRule
	values []string
}

// isModdedLoader сообщает, требует ли загрузчик сгенерированной команды запуска
// (classpath модлоадера), а не ванильного «-jar client.jar». Пусто/vanilla — ваниль.
func isModdedLoader(loader string) bool {
	l := strings.ToLower(strings.TrimSpace(loader))
	return l != "" && l != "vanilla"
}

// buildAndSaveLaunchCommands собирает корректную команду запуска для всех трёх
// ОС из version JSON (с учётом inheritsFrom для загрузчиков) и сохраняет их в
// профиль. Это заменяет ручные шаблоны вроде «-jar client.jar».
func (s Service) buildAndSaveLaunchCommands(profile *models.Profile) error {
	root := s.filesRoot(*profile)

	launchID, err := resolveLaunchVersionID(root, profile.GameVersion, profile.Loader)
	if err != nil {
		return err
	}
	merged, err := loadMergedVersion(root, launchID)
	if err != nil {
		return err
	}
	if merged.MainClass == "" {
		return fmt.Errorf("в version JSON %s не указан mainClass", launchID)
	}
	if merged.AssetIndex == nil || merged.AssetIndex.ID == "" {
		return fmt.Errorf("в version JSON %s не найден assetIndex", launchID)
	}

	// Какой клиентский jar класть в classpath. Для ванили — полный клиент версии.
	// Для модовых загрузчиков (NeoForge/Forge) полный ванильный jar добавлять
	// нельзя — он становится авто-модулем и конфликтует с модулем `minecraft`,
	// который загрузчик собирает из пропатченного клиента. Вместо него нужен
	// client-extra (только ресурсы), а классы клиента предоставляет сам загрузчик.
	clientEntry := "versions/" + profile.GameVersion + "/" + profile.GameVersion + ".jar"
	if isModdedLoader(profile.Loader) {
		extra, err := locateClientExtra(root)
		if err != nil {
			return err
		}
		clientEntry = extra
	}

	for key, target := range launchTargets {
		command := buildCommandForTarget(merged, clientEntry, launchID, target)
		switch key {
		case "windows":
			profile.LaunchCommandWindows = command
		case "linux":
			profile.LaunchCommandLinux = command
		case "macos":
			profile.LaunchCommandMacOS = command
		}
	}
	return nil
}

// resolveLaunchVersionID определяет id версии, которую нужно запускать: для
// ванили это просто gameVersion, для загрузчика — id профиля с inheritsFrom.
func resolveLaunchVersionID(root, gameVersion, loader string) (string, error) {
	if l := strings.ToLower(strings.TrimSpace(loader)); l == "" || l == "vanilla" {
		return gameVersion, nil
	}

	versionsDir := filepath.Join(root, "versions")
	entries, err := os.ReadDir(versionsDir)
	if err != nil {
		return "", fmt.Errorf("не удалось прочитать каталог versions: %w", err)
	}

	candidates := make([]string, 0)
	for _, entry := range entries {
		if !entry.IsDir() || entry.Name() == gameVersion {
			continue
		}
		jsonPath := filepath.Join(versionsDir, entry.Name(), entry.Name()+".json")
		data, err := os.ReadFile(jsonPath)
		if err != nil {
			continue
		}
		var parsed versionJSON
		if err := json.Unmarshal(data, &parsed); err != nil {
			continue
		}
		if parsed.InheritsFrom == gameVersion {
			candidates = append(candidates, entry.Name())
		}
	}
	if len(candidates) == 0 {
		return "", fmt.Errorf("не найден version JSON загрузчика %s для %s — клиент подготовлен не полностью", loader, gameVersion)
	}

	// Предпочитаем кандидата, чьё имя содержит имя загрузчика (neoforge/fabric/...).
	loaderKey := strings.ToLower(strings.TrimSpace(loader))
	sort.Strings(candidates)
	for _, candidate := range candidates {
		if strings.Contains(strings.ToLower(candidate), loaderKey) {
			return candidate, nil
		}
	}
	return candidates[0], nil
}

// loadMergedVersion загружает version JSON и, если он наследуется от другой
// версии (inheritsFrom), объединяет их по правилам Minecraft.
func loadMergedVersion(root, id string) (versionJSON, error) {
	jsonPath := filepath.Join(root, "versions", id, id+".json")
	data, err := os.ReadFile(jsonPath)
	if err != nil {
		return versionJSON{}, fmt.Errorf("не удалось прочитать version JSON %s: %w", id, err)
	}
	var child versionJSON
	if err := json.Unmarshal(data, &child); err != nil {
		return versionJSON{}, fmt.Errorf("повреждён version JSON %s: %w", id, err)
	}
	if child.InheritsFrom == "" {
		return child, nil
	}

	parent, err := loadMergedVersion(root, child.InheritsFrom)
	if err != nil {
		return versionJSON{}, err
	}
	return mergeVersions(parent, child), nil
}

func mergeVersions(parent, child versionJSON) versionJSON {
	merged := parent

	if child.ID != "" {
		merged.ID = child.ID
	}
	if child.Type != "" {
		merged.Type = child.Type
	}
	if child.MainClass != "" {
		merged.MainClass = child.MainClass
	}
	if child.AssetIndex != nil {
		merged.AssetIndex = child.AssetIndex
	}
	if child.Assets != "" {
		merged.Assets = child.Assets
	}

	// Библиотеки дочерней версии имеют приоритет над родительскими с тем же
	// group:artifact:classifier; порядок — сначала дочерние, затем оставшиеся
	// родительские.
	seen := make(map[string]bool)
	libs := make([]versionLib, 0, len(parent.Libraries)+len(child.Libraries))
	for _, lib := range child.Libraries {
		seen[libraryKey(lib.Name)] = true
		libs = append(libs, lib)
	}
	for _, lib := range parent.Libraries {
		if seen[libraryKey(lib.Name)] {
			continue
		}
		libs = append(libs, lib)
	}
	merged.Libraries = libs

	// Аргументы: родительские идут первыми, дочерние дописываются.
	mergedArgs := &versionArgs{}
	if parent.Arguments != nil {
		mergedArgs.JVM = append(mergedArgs.JVM, parent.Arguments.JVM...)
		mergedArgs.Game = append(mergedArgs.Game, parent.Arguments.Game...)
	}
	if child.Arguments != nil {
		mergedArgs.JVM = append(mergedArgs.JVM, child.Arguments.JVM...)
		mergedArgs.Game = append(mergedArgs.Game, child.Arguments.Game...)
	}
	merged.Arguments = mergedArgs

	return merged
}

func buildCommandForTarget(version versionJSON, clientEntry, launchID string, target launchTarget) string {
	placeholders := map[string]string{
		"library_directory":   "libraries",
		"classpath_separator": target.separator,
		"classpath":           "",
		"natives_directory":   "natives",
		"version_name":        launchID,
		"assets_root":         "assets",
		"assets_index_name":   version.AssetIndex.ID,
		"version_type":        firstNonEmpty(version.Type, "release"),
		"launcher_name":       target.launcherName,
		"launcher_version":    "1.0",
		"clientid":            "0",
		"auth_xuid":           "0",
		"user_type":           "msa",
		// Динамические значения подставит сам лаунчер при запуске.
		"auth_player_name":  "{login}",
		"auth_uuid":         "{uuid}",
		"auth_access_token": "{access_token}",
		"game_directory":    "{game_dir}",
	}

	modulePathExclusions := modulePathClasspathExclusions(
		resolveArgs(version.Arguments.JVM, target, placeholders),
		target.separator,
	)
	classpath := buildClasspath(version, clientEntry, target, modulePathExclusions)
	placeholders["classpath"] = classpath

	tokens := make([]string, 0, 64)
	// Команда начинается с плейсхолдеров лаунчера: путь к java и доп. JVM-аргументы профиля.
	tokens = append(tokens, "{java}", "{jvm_args}")

	jvmArgs := resolveArgs(version.Arguments.JVM, target, placeholders)
	tokens = append(tokens, jvmArgs...)
	tokens = append(tokens, version.MainClass)
	gameArgs := resolveArgs(version.Arguments.Game, target, placeholders)
	tokens = append(tokens, gameArgs...)

	quoted := make([]string, 0, len(tokens))
	for _, token := range tokens {
		quoted = append(quoted, quoteToken(token))
	}
	return strings.Join(quoted, " ")
}

func buildClasspath(version versionJSON, clientEntry string, target launchTarget, excluded map[string]bool) string {
	entries := make([]string, 0, len(version.Libraries)+1)
	seen := make(map[string]bool)

	for _, lib := range version.Libraries {
		if !rulesAllow(lib.Rules, target.osName) {
			continue
		}
		path := libraryPath(lib)
		if path == "" || seen[path] {
			continue
		}
		if excluded[normalizeClasspathEntry(path)] {
			continue
		}
		seen[path] = true
		entries = append(entries, path)
	}

	// Клиентский jar (полный ваниль для vanilla либо client-extra для загрузчиков).
	if clientEntry != "" && !seen[clientEntry] && !excluded[normalizeClasspathEntry(clientEntry)] {
		entries = append(entries, clientEntry)
	}

	return strings.Join(entries, target.separator)
}

func modulePathClasspathExclusions(args []string, separator string) map[string]bool {
	excluded := make(map[string]bool)
	for index := 0; index < len(args); index++ {
		arg := args[index]
		modulePath := ""
		switch {
		case arg == "-p" || arg == "--module-path":
			if index+1 >= len(args) {
				continue
			}
			index++
			modulePath = args[index]
		case strings.HasPrefix(arg, "--module-path="):
			modulePath = strings.TrimPrefix(arg, "--module-path=")
		default:
			continue
		}

		for _, entry := range strings.Split(modulePath, separator) {
			entry = normalizeClasspathEntry(entry)
			if entry != "" {
				excluded[entry] = true
			}
		}
	}
	return excluded
}

func normalizeClasspathEntry(entry string) string {
	entry = strings.TrimSpace(strings.Trim(entry, `"'`))
	entry = filepath.ToSlash(entry)
	for strings.HasPrefix(entry, "./") {
		entry = strings.TrimPrefix(entry, "./")
	}
	if index := strings.Index(entry, "libraries/"); index >= 0 {
		entry = entry[index:]
	}
	return entry
}

// locateClientExtra ищет jar с ресурсами клиента (client-...-extra.jar), который
// устанавливает Forge/NeoForge. В classpath модовой сборки идёт именно он, а не
// полный ванильный клиент.
func locateClientExtra(root string) (string, error) {
	clientDir := filepath.Join(root, "libraries", "net", "minecraft", "client")
	versions, err := os.ReadDir(clientDir)
	if err != nil {
		return "", fmt.Errorf("не найден каталог net/minecraft/client — клиент загрузчика подготовлен не полностью: %w", err)
	}
	for _, version := range versions {
		if !version.IsDir() {
			continue
		}
		files, err := os.ReadDir(filepath.Join(clientDir, version.Name()))
		if err != nil {
			continue
		}
		for _, file := range files {
			name := file.Name()
			if strings.HasSuffix(name, "-extra.jar") {
				return "libraries/net/minecraft/client/" + version.Name() + "/" + name, nil
			}
		}
	}
	return "", fmt.Errorf("не найден client-extra jar в %s — переустановите клиент загрузчика", clientDir)
}

// resolveArgs разворачивает список аргументов version JSON: отбрасывает записи,
// чьи правила не подходят под целевую ОС или требуют недоступных features, и
// подставляет значения плейсхолдеров ${...}.
func resolveArgs(raw []json.RawMessage, target launchTarget, placeholders map[string]string) []string {
	result := make([]string, 0, len(raw))
	for _, item := range raw {
		token := parseArgToken(item)
		if !rulesAllow(token.rules, target.osName) {
			continue
		}
		for _, value := range token.values {
			result = append(result, substitutePlaceholders(value, placeholders))
		}
	}
	return result
}

func parseArgToken(raw json.RawMessage) argToken {
	var asString string
	if err := json.Unmarshal(raw, &asString); err == nil {
		return argToken{values: []string{asString}}
	}

	var asObject struct {
		Rules []versionRule   `json:"rules"`
		Value json.RawMessage `json:"value"`
	}
	if err := json.Unmarshal(raw, &asObject); err != nil {
		return argToken{}
	}

	token := argToken{rules: asObject.Rules}
	var single string
	if err := json.Unmarshal(asObject.Value, &single); err == nil {
		token.values = []string{single}
		return token
	}
	var many []string
	if err := json.Unmarshal(asObject.Value, &many); err == nil {
		token.values = many
	}
	return token
}

// rulesAllow вычисляет, разрешён ли элемент для указанной ОС. Features считаем
// всегда выключенными (демо-режим, нестандартное разрешение, quick play и т.п.),
// поэтому записи, завязанные на features, отбрасываются.
func rulesAllow(rules []versionRule, osName string) bool {
	if len(rules) == 0 {
		return true
	}
	allowed := false
	for _, rule := range rules {
		if len(rule.Features) > 0 {
			// Условие на features — для нас не выполняется, правило не применяется.
			continue
		}
		if rule.OS != nil {
			if rule.OS.Name != "" && rule.OS.Name != osName {
				continue
			}
			// Считаем архитектуру x64; правила для x86/arm не применяются.
			if rule.OS.Arch != "" && rule.OS.Arch != "x64" {
				continue
			}
		}
		allowed = rule.Action == "allow"
	}
	return allowed
}

func substitutePlaceholders(value string, placeholders map[string]string) string {
	for key, replacement := range placeholders {
		value = strings.ReplaceAll(value, "${"+key+"}", replacement)
	}
	return value
}

func libraryPath(lib versionLib) string {
	if lib.Downloads != nil && lib.Downloads.Artifact != nil && lib.Downloads.Artifact.Path != "" {
		return "libraries/" + filepath.ToSlash(lib.Downloads.Artifact.Path)
	}
	path, err := mavenArtifactPath(lib.Name)
	if err != nil {
		return ""
	}
	return "libraries/" + filepath.ToSlash(path)
}

// libraryKey формирует ключ дедупликации group:artifact:classifier (без версии).
func libraryKey(name string) string {
	clean := name
	if idx := strings.IndexByte(clean, '@'); idx >= 0 {
		clean = clean[:idx]
	}
	parts := strings.Split(clean, ":")
	group := ""
	artifact := ""
	classifier := ""
	if len(parts) > 0 {
		group = parts[0]
	}
	if len(parts) > 1 {
		artifact = parts[1]
	}
	if len(parts) > 3 {
		classifier = parts[3]
	}
	return group + ":" + artifact + ":" + classifier
}

func quoteToken(token string) string {
	// {jvm_args} обрабатывается лаунчером особым образом и должен остаться как есть.
	if token == "{jvm_args}" {
		return token
	}
	if token == "" || strings.ContainsAny(token, " \t") || strings.Contains(token, "{") {
		return "\"" + token + "\""
	}
	return token
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if value != "" {
			return value
		}
	}
	return ""
}
