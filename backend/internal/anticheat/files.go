package anticheat

import (
	"context"
	"log/slog"
	"strings"
	"time"

	"launcher-backend/internal/models"
)

// ReportedFile — один jar из mods/ игрока: путь относительно игровой папки и SHA-256.
// Missing — jar, из которого классы уже загрузились, но которого на диске больше нет
// (чит, удаливший себя после загрузки); хеш посчитать не с чего, сверять нечего.
type ReportedFile struct {
	Path    string `json:"path"`
	Sha256  string `json:"sha256"`
	Missing bool   `json:"missing"`
}

const (
	// unknownModType — тип детекта «в mods/ лежит jar, которого нет ни в одной сборке».
	// Блэклист имён ловит только ИЗВЕСТНЫЕ читы; здесь работает whitelist: всё, чего
	// админ не публиковал, считается посторонним независимо от названия.
	unknownModType = "unknown-mod"
	// allowedHashesTTL — окно кэша множества разрешённых хешей. Набор меняется только
	// при пересканировании сборки в дашборде, а запрос идёт на каждый старт игры.
	allowedHashesTTL = 60 * time.Second
	// maxReportedFiles — потолок файлов в одном отчёте (защита от флуда БД детектами).
	maxReportedFiles = 512
)

// SetEnforceUnknownMods оставлен для совместимости конфигурации. Несовпадение сборки
// теперь всегда закрывает игру, но никогда не выдаёт автоматический бан.
func (s *Service) SetEnforceUnknownMods(_ bool) {}

// CheckFiles сверяет инвентарь mods/ игрока с хешами файлов, опубликованных админом
// в сборках, и возвращает решение о kick.
//
// Зачем: лаунчер удаляет посторонние файлы при синке (cleanup_unmanaged_files), но
// между cleanup и чтением mods/ модлоадером есть окно в десятки секунд — туда и
// подкидывают чит. Проверка изнутри JVM смотрит на диск уже ПОСЛЕ старта игры, а
// сверку делает сервер: у агента нет списка разрешённых хешей, подделать нечего.
func (s *Service) CheckFiles(ctx context.Context, claims LaunchClaims, files []ReportedFile) (bool, error) {
	allowed, err := s.allowedFileHashes(ctx)
	if err != nil {
		return false, err
	}
	// Пустой набор — БД пуста или запрос деградировал: fail-open, иначе одним махом
	// кикнем всех играющих.
	if len(allowed) == 0 {
		slog.Warn("anticheat: список хешей сборок пуст, проверка mods/ пропущена")
		return false, nil
	}

	// Сначала собираем все посторонние файлы, и только потом пишем ОДИН детект на отчёт.
	// Иначе один запрос превращался в до maxReportedFiles записей БД и столько же
	// Telegram-алертов (дедуп по сигнатуре не спасает — сигнатуры разные).
	var bad []ReportedFile
	for _, f := range files {
		hash := strings.ToLower(strings.TrimSpace(f.Sha256))
		if !f.Missing {
			if len(hash) != 64 {
				continue // не хеш — мусор от поддельного клиента
			}
			if _, ok := allowed[hash]; ok {
				continue
			}
		}
		f.Sha256 = hash
		bad = append(bad, f)
	}
	if len(bad) == 0 {
		return false, nil
	}

	names := make([]string, 0, len(bad))
	for _, f := range bad[:min(len(bad), 20)] {
		names = append(names, baseName(f.Path))
	}
	first := bad[0]
	severity, confidence, err := s.RecordDetection(ctx, claims, DetectionInput{
		Source:    "java",
		Type:      unknownModType,
		Signature: baseName(first.Path),
		Details: map[string]any{
			"name":    baseName(first.Path),
			"path":    truncate(first.Path, 512),
			"hash":    first.Sha256,
			"missing": first.Missing,
			"names":   names,
			"count":   len(bad),
		},
	})
	if err != nil {
		return false, err
	}
	kick, _ := s.EvaluateKick(claims, severity, confidence, unknownModType)
	return kick, nil
}

// allowedFileHashes — множество SHA-256 всех файлов, опубликованных в сборках.
// Сверяем по хешу, а не по пути: переименованный игроком легальный мод не должен
// давать ложный детект, а посторонний jar не спрячется под именем легального.
func (s *Service) allowedFileHashes(ctx context.Context) (map[string]struct{}, error) {
	s.allowedMu.Lock()
	defer s.allowedMu.Unlock()
	if s.allowedHashes != nil && s.now().Sub(s.allowedAt) < allowedHashesTTL {
		return s.allowedHashes, nil
	}
	var hashes []string
	if err := s.db.WithContext(ctx).Model(&models.GameFile{}).
		Distinct().Pluck("hash_sha256", &hashes).Error; err != nil {
		return nil, err
	}
	set := make(map[string]struct{}, len(hashes))
	for _, h := range hashes {
		if h = strings.ToLower(strings.TrimSpace(h)); len(h) == 64 {
			set[h] = struct{}{}
		}
	}
	s.allowedHashes, s.allowedAt = set, s.now()
	return set, nil
}

// baseName — имя файла из относительного пути (сигнатура детекта должна читаться
// в панели, а не быть путём на диске игрока).
func baseName(path string) string {
	path = strings.ReplaceAll(path, "\\", "/")
	if i := strings.LastIndex(path, "/"); i >= 0 {
		path = path[i+1:]
	}
	return truncate(path, maxDetectionField)
}
