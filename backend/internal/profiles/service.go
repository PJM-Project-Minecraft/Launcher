package profiles

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"strings"
	"time"

	"launcher-backend/internal/models"

	"github.com/google/uuid"
	"gorm.io/gorm"
)

type Service struct {
	db          *gorm.DB
	storageRoot string
	// cdnBase — см. config.ProfileCDNBase. Пусто → download_url остаётся относительным на бэкенд.
	cdnBase string
}

var defaultPreservePaths = []string{
	"saves/",
	"resourcepacks/",
	"shaderpacks/",
	"screenshots/",
	"logs/",
	"crash-reports/",
	"options.txt",
	"optionsof.txt",
	"servers.dat",
}

type ProfileRequest struct {
	Name                 string   `json:"name"`
	Slug                 string   `json:"slug"`
	Description          string   `json:"description"`
	Loader               string   `json:"loader"`
	GameVersion          string   `json:"gameVersion"`
	LoaderVersion        string   `json:"loaderVersion"`
	JavaVersion          int      `json:"javaVersion"`
	JVMArgs              string   `json:"jvmArgs"`
	IconURL              string   `json:"iconUrl"`
	JavaPathWindows      string   `json:"javaPathWindows"`
	JavaPathLinux        string   `json:"javaPathLinux"`
	JavaPathMacOS        string   `json:"javaPathMacos"`
	LaunchCommandWindows string   `json:"launchCommandWindows"`
	LaunchCommandLinux   string   `json:"launchCommandLinux"`
	LaunchCommandMacOS   string   `json:"launchCommandMacos"`
	PreservePaths        []string `json:"preservePaths"`
	IsActive             *bool    `json:"isActive"`
}

type ProfileSummary struct {
	ID              string     `json:"id"`
	Name            string     `json:"name"`
	Slug            string     `json:"slug"`
	Description     string     `json:"description"`
	Loader          string     `json:"loader"`
	GameVersion     string     `json:"gameVersion"`
	LoaderVersion   string     `json:"loaderVersion"`
	JavaVersion     int        `json:"javaVersion"`
	JVMArgs         string     `json:"jvmArgs"`
	IconURL         string     `json:"iconUrl"`
	JavaPathWindows string     `json:"javaPathWindows"`
	JavaPathLinux   string     `json:"javaPathLinux"`
	JavaPathMacOS   string     `json:"javaPathMacos"`
	LaunchWindows   string     `json:"launchCommandWindows"`
	LaunchLinux     string     `json:"launchCommandLinux"`
	LaunchMacOS     string     `json:"launchCommandMacos"`
	PreservePaths   []string   `json:"preservePaths"`
	ManifestVersion int        `json:"manifestVersion"`
	ManifestUpdated *time.Time `json:"manifestUpdatedAt"`
	IsActive        bool       `json:"isActive"`
	FileCount       int64      `json:"fileCount"`
	TotalSize       int64      `json:"totalSize"`
	ClientPrepared  bool       `json:"clientPrepared"`
	ClientStatus    string     `json:"clientStatus"`
	CreatedAt       time.Time  `json:"createdAt"`
	UpdatedAt       time.Time  `json:"updatedAt"`
}

type ManifestProfile struct {
	ID                   string     `json:"id"`
	Name                 string     `json:"name"`
	Slug                 string     `json:"slug"`
	Description          string     `json:"description"`
	Loader               string     `json:"loader"`
	GameVersion          string     `json:"gameVersion"`
	LoaderVersion        string     `json:"loaderVersion"`
	JavaVersion          int        `json:"javaVersion"`
	JVMArgs              string     `json:"jvmArgs"`
	IconURL              string     `json:"iconUrl"`
	JavaPathWindows      string     `json:"javaPathWindows"`
	JavaPathLinux        string     `json:"javaPathLinux"`
	JavaPathMacOS        string     `json:"javaPathMacos"`
	LaunchCommandWindows string     `json:"launchCommandWindows"`
	LaunchCommandLinux   string     `json:"launchCommandLinux"`
	LaunchCommandMacOS   string     `json:"launchCommandMacos"`
	ManifestVersion      int        `json:"manifestVersion"`
	ManifestUpdatedAt    *time.Time `json:"manifestUpdatedAt"`
}

type ManifestFile struct {
	ID          string `json:"id"`
	Name        string `json:"name"`
	Path        string `json:"path"`
	DownloadURL string `json:"downloadUrl"`
	HashSHA256  string `json:"hashSha256"`
	Size        int64  `json:"size"`
	FileType    string `json:"fileType"`
}

type Manifest struct {
	Profile       ManifestProfile `json:"profile"`
	Files         []ManifestFile  `json:"files"`
	PreservePaths []string        `json:"preservePaths"`
	FileCount     int             `json:"fileCount"`
	TotalSize     int64           `json:"totalSize"`
}

type ScanResult struct {
	Profile   ProfileSummary `json:"profile"`
	FileCount int64          `json:"fileCount"`
	TotalSize int64          `json:"totalSize"`
}

type FileDownload struct {
	AbsolutePath string
	File         models.GameFile
}

func NewService(db *gorm.DB, storageRoot string, cdnBase string) Service {
	return Service{db: db, storageRoot: storageRoot, cdnBase: strings.TrimRight(cdnBase, "/")}
}

func (s Service) ListActive(ctx context.Context) ([]ProfileSummary, error) {
	var items []models.Profile
	if err := s.db.WithContext(ctx).
		Where("is_active = ?", true).
		Order("created_at asc").
		Find(&items).Error; err != nil {
		return nil, err
	}
	return s.toSummaries(ctx, items)
}

func (s Service) ListAll(ctx context.Context) ([]ProfileSummary, error) {
	var items []models.Profile
	if err := s.db.WithContext(ctx).Order("created_at asc").Find(&items).Error; err != nil {
		return nil, err
	}
	return s.toSummaries(ctx, items)
}

func (s Service) Create(ctx context.Context, req ProfileRequest) (ProfileSummary, error) {
	profile, err := profileFromRequest(models.Profile{ID: uuid.NewString()}, req)
	if err != nil {
		return ProfileSummary{}, err
	}

	if err := s.db.WithContext(ctx).Create(&profile).Error; err != nil {
		return ProfileSummary{}, err
	}
	if err := os.MkdirAll(s.filesRoot(profile), 0755); err != nil {
		return ProfileSummary{}, err
	}
	return s.summary(ctx, profile)
}

func (s Service) Update(ctx context.Context, id string, req ProfileRequest) (ProfileSummary, error) {
	var profile models.Profile
	if err := s.db.WithContext(ctx).First(&profile, "id = ?", id).Error; err != nil {
		return ProfileSummary{}, err
	}

	oldSlug := profile.Slug
	updated, err := profileFromRequest(profile, req)
	if err != nil {
		return ProfileSummary{}, err
	}

	if oldSlug != "" && oldSlug != updated.Slug {
		if err := s.renameProfileFolder(oldSlug, updated.Slug); err != nil {
			return ProfileSummary{}, err
		}
	}

	if err := s.db.WithContext(ctx).Save(&updated).Error; err != nil {
		return ProfileSummary{}, err
	}
	if err := os.MkdirAll(s.filesRoot(updated), 0755); err != nil {
		return ProfileSummary{}, err
	}
	return s.summary(ctx, updated)
}

func (s Service) Delete(ctx context.Context, id string) error {
	return s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		if err := tx.Where("profile_id = ?", id).Delete(&models.GameFile{}).Error; err != nil {
			return err
		}
		return tx.Delete(&models.Profile{}, "id = ?", id).Error
	})
}

func (s Service) Scan(ctx context.Context, id string) (ScanResult, error) {
	var profile models.Profile
	if err := s.db.WithContext(ctx).First(&profile, "id = ?", id).Error; err != nil {
		return ScanResult{}, err
	}
	if err := validateSlug(profile.Slug); err != nil {
		return ScanResult{}, err
	}

	root := s.filesRoot(profile)
	if err := os.MkdirAll(root, 0755); err != nil {
		return ScanResult{}, err
	}
	if err := rejectSymlink(root); err != nil {
		return ScanResult{}, err
	}

	files := make([]models.GameFile, 0)
	var totalSize int64
	preservePaths := profilePreservePaths(profile)

	err := filepath.WalkDir(root, func(path string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if path == root {
			return nil
		}
		rel, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		rel = filepath.ToSlash(rel)
		rel, err = safeRelativePath(rel)
		if err != nil {
			return err
		}
		if preservePathMatches(rel, preservePaths) {
			if entry.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		if entry.Type()&os.ModeSymlink != 0 {
			return fmt.Errorf("symlink is not allowed in profile files: %s", path)
		}
		if entry.IsDir() {
			return nil
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		if !info.Mode().IsRegular() {
			return nil
		}
		if isInstallerOnlyFile(rel) {
			// Утилиты установщика загрузчика нужны только при подготовке клиента,
			// в рантайме не используются — не включаем их в манифест.
			return nil
		}

		hash, err := hashFile(path)
		if err != nil {
			return err
		}

		totalSize += info.Size()
		files = append(files, models.GameFile{
			ID:         uuid.NewString(),
			ProfileID:  profile.ID,
			Name:       filepath.Base(rel),
			Path:       rel,
			URL:        "/api/profiles/" + profile.ID + "/files/" + escapePath(rel),
			HashSHA256: hash,
			Size:       info.Size(),
			FileType:   inferFileType(rel),
		})
		return nil
	})
	if err != nil {
		return ScanResult{}, err
	}

	now := time.Now().UTC()
	profile.ManifestVersion++
	profile.ManifestUpdatedAt = &now

	if err := s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		if err := tx.Where("profile_id = ?", profile.ID).Delete(&models.GameFile{}).Error; err != nil {
			return err
		}
		if len(files) > 0 {
			// Профиль может содержать тысячи файлов (assets/libraries), поэтому
			// вставляем пачками — одиночный INSERT упирается в
			// SQLITE_MAX_VARIABLE_NUMBER ("too many SQL variables").
			if err := tx.CreateInBatches(&files, 100).Error; err != nil {
				return err
			}
		}
		return tx.Save(&profile).Error
	}); err != nil {
		return ScanResult{}, err
	}

	summary, err := s.summary(ctx, profile)
	if err != nil {
		return ScanResult{}, err
	}
	return ScanResult{
		Profile:   summary,
		FileCount: int64(len(files)),
		TotalSize: totalSize,
	}, nil
}

// DriftResult — итог дешёвой сверки storage с манифестом в БД: пути, размеры и
// mtime, без хэширования. Ловит «обновил файлы профиля, забыл нажать
// „Сканировать файлы"» — до сканирования лаунчеры игроков падают на
// hash mismatch. Подмена файла тем же размером со старым mtime (rsync -a)
// эвристикой не ловится — это осознанный компромисс ради скорости.
type DriftResult struct {
	// Scanned — манифест хоть раз собирался.
	Scanned bool `json:"scanned"`
	Drifted bool `json:"drifted"`
	// Added — файлы на диске, которых нет в манифесте.
	Added int `json:"added"`
	// Removed — файлы манифеста, пропавшие с диска.
	Removed int `json:"removed"`
	// Changed — размер отличается или mtime новее последнего сканирования.
	Changed int `json:"changed"`
}

func (s Service) Drift(ctx context.Context, id string) (DriftResult, error) {
	var profile models.Profile
	if err := s.db.WithContext(ctx).First(&profile, "id = ?", id).Error; err != nil {
		return DriftResult{}, err
	}
	if err := validateSlug(profile.Slug); err != nil {
		return DriftResult{}, err
	}

	var dbFiles []models.GameFile
	if err := s.db.WithContext(ctx).Where("profile_id = ?", id).Find(&dbFiles).Error; err != nil {
		return DriftResult{}, err
	}
	manifestSizes := make(map[string]int64, len(dbFiles))
	for _, file := range dbFiles {
		manifestSizes[file.Path] = file.Size
	}

	result := DriftResult{Scanned: profile.ManifestUpdatedAt != nil}
	root := s.filesRoot(profile)
	preservePaths := profilePreservePaths(profile)
	seen := make(map[string]bool, len(manifestSizes))

	err := filepath.WalkDir(root, func(path string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil {
			// Папки профиля ещё нет — весь манифест «пропал с диска».
			if path == root && os.IsNotExist(walkErr) {
				return filepath.SkipAll
			}
			return walkErr
		}
		if path == root {
			return nil
		}
		rel, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		rel = filepath.ToSlash(rel)
		rel, err = safeRelativePath(rel)
		if err != nil {
			return err
		}
		if preservePathMatches(rel, preservePaths) {
			if entry.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		if entry.IsDir() {
			return nil
		}
		if entry.Type()&os.ModeSymlink != 0 {
			// Scan симлинк отвергнет — значит storage разошёлся с манифестом.
			result.Added++
			return nil
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		if !info.Mode().IsRegular() || isInstallerOnlyFile(rel) {
			return nil
		}

		size, inManifest := manifestSizes[rel]
		if !inManifest {
			result.Added++
			return nil
		}
		seen[rel] = true
		if info.Size() != size {
			result.Changed++
			return nil
		}
		if profile.ManifestUpdatedAt != nil && info.ModTime().After(*profile.ManifestUpdatedAt) {
			result.Changed++
		}
		return nil
	})
	if err != nil {
		return DriftResult{}, err
	}

	for path := range manifestSizes {
		if !seen[path] {
			result.Removed++
		}
	}
	result.Drifted = result.Added > 0 || result.Removed > 0 || result.Changed > 0
	return result, nil
}

func (s Service) Manifest(ctx context.Context, id string) (Manifest, error) {
	var profile models.Profile
	if err := s.db.WithContext(ctx).
		Where("id = ? AND is_active = ?", id, true).
		First(&profile).Error; err != nil {
		return Manifest{}, err
	}

	var files []models.GameFile
	if err := s.db.WithContext(ctx).
		Where("profile_id = ?", id).
		Order("path asc").
		Find(&files).Error; err != nil {
		return Manifest{}, err
	}

	manifestFiles := make([]ManifestFile, 0, len(files))
	var totalSize int64
	for _, file := range files {
		totalSize += file.Size
		manifestFiles = append(manifestFiles, ManifestFile{
			ID:          file.ID,
			Name:        file.Name,
			Path:        file.Path,
			DownloadURL: s.downloadURL(profile, file.Path),
			HashSHA256:  file.HashSHA256,
			Size:        file.Size,
			FileType:    file.FileType,
		})
	}

	return Manifest{
		Profile:       toManifestProfile(profile),
		Files:         manifestFiles,
		PreservePaths: profilePreservePaths(profile),
		FileCount:     len(manifestFiles),
		TotalSize:     totalSize,
	}, nil
}

func (s Service) Download(ctx context.Context, id string, requestedPath string) (FileDownload, error) {
	rel, err := safeRelativePath(requestedPath)
	if err != nil {
		return FileDownload{}, err
	}

	var profile models.Profile
	if err := s.db.WithContext(ctx).
		Where("id = ? AND is_active = ?", id, true).
		First(&profile).Error; err != nil {
		return FileDownload{}, err
	}

	var file models.GameFile
	if err := s.db.WithContext(ctx).
		Where("profile_id = ? AND path = ?", profile.ID, rel).
		First(&file).Error; err != nil {
		return FileDownload{}, err
	}

	absolutePath, err := safeJoin(s.filesRoot(profile), file.Path)
	if err != nil {
		return FileDownload{}, err
	}
	if err := rejectSymlink(absolutePath); err != nil {
		return FileDownload{}, err
	}
	return FileDownload{AbsolutePath: absolutePath, File: file}, nil
}

func (s Service) summary(ctx context.Context, profile models.Profile) (ProfileSummary, error) {
	var count int64
	var totalSize int64
	row := s.db.WithContext(ctx).
		Model(&models.GameFile{}).
		Select("count(*), coalesce(sum(size), 0)").
		Where("profile_id = ?", profile.ID).
		Row()
	if err := row.Scan(&count, &totalSize); err != nil {
		return ProfileSummary{}, err
	}
	clientPrepared := s.clientPrepared(profile)
	clientStatus := "missing"
	if clientPrepared {
		clientStatus = "ready"
	}
	return toSummary(profile, count, totalSize, clientPrepared, clientStatus), nil
}

func (s Service) toSummaries(ctx context.Context, profiles []models.Profile) ([]ProfileSummary, error) {
	summaries := make([]ProfileSummary, 0, len(profiles))
	for _, profile := range profiles {
		summary, err := s.summary(ctx, profile)
		if err != nil {
			return nil, err
		}
		summaries = append(summaries, summary)
	}
	return summaries, nil
}

func (s Service) filesRoot(profile models.Profile) string {
	return filepath.Join(s.storageRoot, profile.Slug, "files")
}

func (s Service) renameProfileFolder(oldSlug, newSlug string) error {
	if oldSlug == newSlug {
		return nil
	}
	if err := validateSlug(newSlug); err != nil {
		return err
	}

	oldRoot := filepath.Join(s.storageRoot, oldSlug)
	newRoot := filepath.Join(s.storageRoot, newSlug)
	if _, err := os.Stat(oldRoot); errors.Is(err, os.ErrNotExist) {
		return nil
	} else if err != nil {
		return err
	}
	if _, err := os.Stat(newRoot); err == nil {
		return fmt.Errorf("profile storage folder already exists: %s", newSlug)
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return os.Rename(oldRoot, newRoot)
}

func profileFromRequest(profile models.Profile, req ProfileRequest) (models.Profile, error) {
	profile.Name = strings.TrimSpace(req.Name)
	profile.Slug = normalizeSlug(req.Slug)
	if profile.Slug == "" {
		profile.Slug = normalizeSlug(profile.Name)
	}
	profile.Description = strings.TrimSpace(req.Description)
	profile.Loader = strings.TrimSpace(req.Loader)
	profile.GameVersion = strings.TrimSpace(req.GameVersion)
	profile.LoaderVersion = strings.TrimSpace(req.LoaderVersion)
	profile.JavaVersion = req.JavaVersion
	profile.JVMArgs = strings.TrimSpace(req.JVMArgs)
	profile.IconURL = strings.TrimSpace(req.IconURL)
	profile.JavaPathWindows = strings.TrimSpace(req.JavaPathWindows)
	profile.JavaPathLinux = strings.TrimSpace(req.JavaPathLinux)
	profile.JavaPathMacOS = strings.TrimSpace(req.JavaPathMacOS)
	// Для модовых загрузчиков команду запуска генерирует buildAndSaveLaunchCommands
	// («Подготовить клиент»). Ванильный плейсхолдер из формы дашборда (-jar client.jar)
	// для NeoForge/Forge нерабочий, поэтому при сохранении НЕ затираем им уже
	// сгенерированную команду — сохраняем существующую. Для ванили берём из запроса.
	if isModdedLoader(strings.TrimSpace(req.Loader)) {
		// profile.LaunchCommand* уже содержат значения из БД (Update) или пусты (Create).
	} else {
		profile.LaunchCommandWindows = strings.TrimSpace(req.LaunchCommandWindows)
		profile.LaunchCommandLinux = strings.TrimSpace(req.LaunchCommandLinux)
		profile.LaunchCommandMacOS = strings.TrimSpace(req.LaunchCommandMacOS)
	}
	preservePaths, err := normalizePreservePaths(req.PreservePaths)
	if err != nil {
		return models.Profile{}, err
	}
	profile.PreservePaths = preservePaths

	if profile.Loader == "" {
		profile.Loader = "vanilla"
	}
	if profile.GameVersion == "" {
		profile.GameVersion = "unknown"
	}
	if profile.JavaVersion == 0 {
		profile.JavaVersion = javaVersionForMinecraft(profile.GameVersion)
	}
	if profile.JavaPathWindows == "" {
		profile.JavaPathWindows = "runtime/windows-x64/bin/java.exe"
	}
	if profile.JavaPathLinux == "" {
		profile.JavaPathLinux = "runtime/linux/bin/java"
	}
	if profile.JavaPathMacOS == "" {
		profile.JavaPathMacOS = "runtime/mac-os/jre.bundle/Contents/Home/bin/java"
	}
	if req.IsActive != nil {
		profile.IsActive = *req.IsActive
	} else if profile.ID != "" && profile.CreatedAt.IsZero() {
		profile.IsActive = true
	}

	if profile.Name == "" {
		return models.Profile{}, errors.New("name is required")
	}
	if err := validateSlug(profile.Slug); err != nil {
		return models.Profile{}, err
	}
	return profile, nil
}

func toSummary(profile models.Profile, fileCount int64, totalSize int64, clientPrepared bool, clientStatus string) ProfileSummary {
	return ProfileSummary{
		ID:              profile.ID,
		Name:            profile.Name,
		Slug:            profile.Slug,
		Description:     profile.Description,
		Loader:          profile.Loader,
		GameVersion:     profile.GameVersion,
		LoaderVersion:   profile.LoaderVersion,
		JavaVersion:     profile.JavaVersion,
		JVMArgs:         profile.JVMArgs,
		IconURL:         profile.IconURL,
		JavaPathWindows: profile.JavaPathWindows,
		JavaPathLinux:   profile.JavaPathLinux,
		JavaPathMacOS:   profile.JavaPathMacOS,
		LaunchWindows:   profile.LaunchCommandWindows,
		LaunchLinux:     profile.LaunchCommandLinux,
		LaunchMacOS:     profile.LaunchCommandMacOS,
		PreservePaths:   profilePreservePaths(profile),
		ManifestVersion: profile.ManifestVersion,
		ManifestUpdated: profile.ManifestUpdatedAt,
		IsActive:        profile.IsActive,
		FileCount:       fileCount,
		TotalSize:       totalSize,
		ClientPrepared:  clientPrepared,
		ClientStatus:    clientStatus,
		CreatedAt:       profile.CreatedAt,
		UpdatedAt:       profile.UpdatedAt,
	}
}

func (s Service) clientPrepared(profile models.Profile) bool {
	root := s.filesRoot(profile)
	versionDir := filepath.Join(root, "versions", profile.GameVersion)
	if !regularFileExists(filepath.Join(versionDir, profile.GameVersion+".json")) ||
		!regularFileExists(filepath.Join(versionDir, profile.GameVersion+".jar")) {
		return false
	}

	switch strings.ToLower(profile.Loader) {
	case "", "vanilla":
		return true
	case "fabric":
		versionID := "fabric-loader-" + profile.LoaderVersion + "-" + profile.GameVersion
		return regularFileExists(filepath.Join(root, "versions", versionID, versionID+".json"))
	case "quilt":
		versionID := "quilt-loader-" + profile.LoaderVersion + "-" + profile.GameVersion
		return regularFileExists(filepath.Join(root, "versions", versionID, versionID+".json"))
	case "forge":
		return installedLoaderVersionExists(root, profile)
	case "neoforge":
		return installedLoaderVersionExists(root, profile)
	default:
		return false
	}
}

func installedLoaderVersionExists(root string, profile models.Profile) bool {
	loaderKey := strings.ToLower(strings.TrimSpace(profile.Loader))
	loaderVersion := strings.TrimSpace(profile.LoaderVersion)
	versionsDir := filepath.Join(root, "versions")
	entries, err := os.ReadDir(versionsDir)
	if err != nil {
		return false
	}

	for _, entry := range entries {
		if !entry.IsDir() || entry.Name() == profile.GameVersion {
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
		if parsed.InheritsFrom != profile.GameVersion {
			continue
		}
		versionID := parsed.ID
		if versionID == "" {
			versionID = entry.Name()
		}
		normalizedID := strings.ToLower(versionID)
		if loaderKey != "" && !strings.Contains(normalizedID, loaderKey) {
			continue
		}
		if loaderVersion != "" && !strings.Contains(versionID, loaderVersion) {
			continue
		}
		return true
	}
	return false
}

func regularFileExists(path string) bool {
	info, err := os.Stat(path)
	return err == nil && info.Mode().IsRegular()
}

func toManifestProfile(profile models.Profile) ManifestProfile {
	return ManifestProfile{
		ID:                   profile.ID,
		Name:                 profile.Name,
		Slug:                 profile.Slug,
		Description:          profile.Description,
		Loader:               profile.Loader,
		GameVersion:          profile.GameVersion,
		LoaderVersion:        profile.LoaderVersion,
		JavaVersion:          profile.JavaVersion,
		JVMArgs:              profile.JVMArgs,
		IconURL:              profile.IconURL,
		JavaPathWindows:      profile.JavaPathWindows,
		JavaPathLinux:        profile.JavaPathLinux,
		JavaPathMacOS:        profile.JavaPathMacOS,
		LaunchCommandWindows: profile.LaunchCommandWindows,
		LaunchCommandLinux:   profile.LaunchCommandLinux,
		LaunchCommandMacOS:   profile.LaunchCommandMacOS,
		ManifestVersion:      profile.ManifestVersion,
		ManifestUpdatedAt:    profile.ManifestUpdatedAt,
	}
}

func profilePreservePaths(profile models.Profile) []string {
	paths := profile.PreservePaths
	if len(paths) == 0 {
		paths = defaultPreservePaths
	}
	result := make([]string, len(paths))
	copy(result, paths)
	return result
}

func normalizePreservePaths(values []string) ([]string, error) {
	if len(values) == 0 {
		return profileDefaultPreservePaths(), nil
	}

	seen := make(map[string]bool, len(values))
	result := make([]string, 0, len(values))
	for _, value := range values {
		normalized, err := normalizePreservePath(value)
		if err != nil {
			return nil, err
		}
		if seen[normalized] {
			continue
		}
		seen[normalized] = true
		result = append(result, normalized)
	}
	if len(result) == 0 {
		return profileDefaultPreservePaths(), nil
	}
	return result, nil
}

func profileDefaultPreservePaths() []string {
	result := make([]string, len(defaultPreservePaths))
	copy(result, defaultPreservePaths)
	return result
}

func normalizePreservePath(value string) (string, error) {
	raw := strings.TrimSpace(strings.ReplaceAll(value, "\\", "/"))
	if raw == "" {
		return "", errors.New("preserve path is empty")
	}
	if strings.HasPrefix(raw, "/") || strings.Contains(raw, ":") {
		return "", fmt.Errorf("unsafe preserve path: %s", value)
	}

	isDir := strings.HasSuffix(raw, "/")
	cleaned := path.Clean(strings.TrimRight(raw, "/"))
	if cleaned == "." || cleaned == "" || cleaned == ".." || strings.HasPrefix(cleaned, "../") {
		return "", fmt.Errorf("unsafe preserve path: %s", value)
	}

	normalized := cleaned
	if isDir {
		normalized += "/"
	}
	if isReservedPreservePath(normalized) {
		return "", fmt.Errorf("preserve path is reserved: %s", normalized)
	}
	return normalized, nil
}

func isReservedPreservePath(value string) bool {
	root := strings.TrimSuffix(value, "/")
	if index := strings.Index(root, "/"); index >= 0 {
		root = root[:index]
	}
	switch root {
	case "mods", "libraries", "versions", "assets", "runtime":
		return true
	default:
		return false
	}
}

func preservePathMatches(rel string, preservePaths []string) bool {
	rel = strings.TrimPrefix(filepath.ToSlash(rel), "/")
	for _, preservePath := range preservePaths {
		if strings.HasSuffix(preservePath, "/") {
			root := strings.TrimSuffix(preservePath, "/")
			if rel == root || strings.HasPrefix(rel, preservePath) {
				return true
			}
			continue
		}
		if rel == preservePath {
			return true
		}
	}
	return false
}

func normalizeSlug(value string) string {
	value = strings.ToLower(strings.TrimSpace(value))
	var builder strings.Builder
	lastDash := false
	for _, ch := range value {
		valid := (ch >= 'a' && ch <= 'z') || (ch >= '0' && ch <= '9') || ch == '_' || ch == '-'
		if valid {
			builder.WriteRune(ch)
			lastDash = ch == '-'
			continue
		}
		if ch == ' ' || ch == '.' || ch == '/' || ch == '\\' {
			if builder.Len() > 0 && !lastDash {
				builder.WriteByte('-')
				lastDash = true
			}
		}
	}
	return strings.Trim(builder.String(), "-_")
}

func validateSlug(slug string) error {
	if slug == "" {
		return errors.New("slug is required")
	}
	if len(slug) > 80 {
		return errors.New("slug is too long")
	}
	for _, ch := range slug {
		if (ch >= 'a' && ch <= 'z') || (ch >= '0' && ch <= '9') || ch == '-' || ch == '_' {
			continue
		}
		return errors.New("slug may contain only a-z, 0-9, dash and underscore")
	}
	return nil
}

func safeRelativePath(value string) (string, error) {
	value = strings.TrimSpace(value)
	unescaped, err := url.PathUnescape(value)
	if err == nil {
		value = unescaped
	}
	value = strings.TrimPrefix(value, "/")
	value = filepath.Clean(filepath.FromSlash(value))
	if value == "." || value == "" || filepath.IsAbs(value) || strings.HasPrefix(value, ".."+string(os.PathSeparator)) || value == ".." {
		return "", errors.New("unsafe path")
	}
	return filepath.ToSlash(value), nil
}

func safeJoin(root, rel string) (string, error) {
	rel, err := safeRelativePath(rel)
	if err != nil {
		return "", err
	}
	rootAbs, err := filepath.Abs(root)
	if err != nil {
		return "", err
	}
	pathAbs, err := filepath.Abs(filepath.Join(rootAbs, filepath.FromSlash(rel)))
	if err != nil {
		return "", err
	}
	prefix := rootAbs + string(os.PathSeparator)
	if !strings.HasPrefix(pathAbs, prefix) {
		return "", errors.New("unsafe path")
	}
	return pathAbs, nil
}

// downloadURL — откуда лаунчер тянет файл профиля. С заданным cdnBase это прямая
// ссылка на бакет (ключ = <slug>/files/<path>, как в storage), иначе — путь на бэкенд.
// Целостность в обоих случаях держится на SHA-256 из этого же манифеста, который
// лаунчер получает по JWT с бэкенда, — подмена файла в бакете ломает проверку хэша.
func (s Service) downloadURL(profile models.Profile, path string) string {
	if s.cdnBase == "" {
		return "/api/profiles/" + profile.ID + "/files/" + escapePath(path)
	}
	return s.cdnBase + "/" + escapePath(profile.Slug) + "/files/" + escapePath(path)
}

func escapePath(path string) string {
	parts := strings.Split(filepath.ToSlash(path), "/")
	for index, part := range parts {
		parts[index] = url.PathEscape(part)
	}
	return strings.Join(parts, "/")
}

func rejectSymlink(path string) error {
	info, err := os.Lstat(path)
	if err != nil {
		return err
	}
	if info.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("symlink is not allowed: %s", path)
	}
	return nil
}

func hashFile(path string) (string, error) {
	file, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer file.Close()

	hasher := sha256.New()
	if _, err := io.Copy(hasher, file); err != nil {
		return "", err
	}
	return hex.EncodeToString(hasher.Sum(nil)), nil
}

// isInstallerOnlyFile отмечает файлы, которые нужны только установщику загрузчика
// (Forge/NeoForge) на этапе подготовки клиента и не используются при запуске игры.
func isInstallerOnlyFile(path string) bool {
	return strings.HasPrefix(path, "libraries/net/neoforged/installertools/") ||
		strings.HasPrefix(path, "libraries/net/minecraftforge/installertools/")
}

func inferFileType(path string) string {
	switch {
	case strings.HasPrefix(path, "mods/"):
		return "mod"
	case strings.HasPrefix(path, "libraries/"):
		return "library"
	case strings.HasPrefix(path, "assets/"):
		return "asset"
	case strings.HasPrefix(path, "runtime/"):
		return "runtime"
	case strings.HasSuffix(path, ".jar"):
		return "jar"
	default:
		return "game"
	}
}
