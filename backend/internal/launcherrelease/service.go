package launcherrelease

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"launcher-backend/internal/models"

	"github.com/google/uuid"
	"gorm.io/gorm"
)

// AllowedPlatforms — платформы, под которые собирается лаунчер (см. спек).
var AllowedPlatforms = []string{"linux-x64", "windows-x64"}

// maxReleaseFileSize — лимит размера одного бинарника (бэкенд-защита; в проде
// nginx client_max_body_size тоже должен пропускать такой запрос).
const maxReleaseFileSize = 200 << 20

type Service struct {
	db          *gorm.DB
	storageRoot string
}

func NewService(db *gorm.DB, storageRoot string) Service {
	return Service{db: db, storageRoot: storageRoot}
}

// StorageRoot возвращает корень каталога релизов; пустая строка — сервис не
// сконфигурирован (бот скрывает кнопки скачивания по платформам).
func (s Service) StorageRoot() string { return s.storageRoot }

type CreateRequest struct {
	Version   string
	Changelog string
	Mandatory bool
}

// UploadedFile — один бинарник из multipart-формы админки.
type UploadedFile struct {
	Platform string
	FileName string
	Reader   io.Reader
}

// PatchRequest — частичное обновление флагов релиза.
type PatchRequest struct {
	Mandatory *bool `json:"mandatory"`
	IsActive  *bool `json:"isActive"`
}

// UpdateInfo — ответ /api/launcher/update для лаунчера.
type UpdateInfo struct {
	UpdateAvailable bool   `json:"updateAvailable"`
	LatestVersion   string `json:"latestVersion"`
	Mandatory       bool   `json:"mandatory"`
	Changelog       string `json:"changelog"`
	DownloadURL     string `json:"downloadUrl"`
	SHA256          string `json:"sha256"`
	Size            int64  `json:"size"`
}

func isAllowedPlatform(platform string) bool {
	for _, allowed := range AllowedPlatforms {
		if platform == allowed {
			return true
		}
	}
	return false
}

func (s Service) Create(ctx context.Context, req CreateRequest, files []UploadedFile) (models.LauncherRelease, error) {
	version := strings.TrimSpace(req.Version)
	if !ValidVersion(version) {
		return models.LauncherRelease{}, errors.New("версия должна быть в формате X.Y.Z")
	}
	if len(files) == 0 {
		return models.LauncherRelease{}, errors.New("прикрепите бинарник хотя бы для одной платформы")
	}

	var count int64
	if err := s.db.WithContext(ctx).Model(&models.LauncherRelease{}).
		Where("version = ?", version).Count(&count).Error; err != nil {
		return models.LauncherRelease{}, err
	}
	if count > 0 {
		return models.LauncherRelease{}, fmt.Errorf("релиз %s уже существует", version)
	}

	release := models.LauncherRelease{
		ID:        uuid.NewString(),
		Version:   version,
		Changelog: strings.TrimSpace(req.Changelog),
		Mandatory: req.Mandatory,
		IsActive:  true,
	}

	seen := map[string]bool{}
	for _, file := range files {
		if !isAllowedPlatform(file.Platform) {
			return models.LauncherRelease{}, fmt.Errorf("неизвестная платформа: %s", file.Platform)
		}
		if seen[file.Platform] {
			return models.LauncherRelease{}, fmt.Errorf("платформа %s указана дважды", file.Platform)
		}
		seen[file.Platform] = true

		stored, err := s.storeFile(release.ID, version, file)
		if err != nil {
			_ = os.RemoveAll(filepath.Join(s.storageRoot, version))
			return models.LauncherRelease{}, err
		}
		release.Files = append(release.Files, stored)
	}

	if err := s.db.WithContext(ctx).Create(&release).Error; err != nil {
		_ = os.RemoveAll(filepath.Join(s.storageRoot, version))
		return models.LauncherRelease{}, err
	}
	return release, nil
}

// storeFile пишет бинарник на диск, считая SHA-256 на лету.
func (s Service) storeFile(releaseID, version string, file UploadedFile) (models.LauncherReleaseFile, error) {
	dir := filepath.Join(s.storageRoot, version, file.Platform)
	if err := os.MkdirAll(dir, 0755); err != nil {
		return models.LauncherReleaseFile{}, err
	}
	name := filepath.Base(strings.TrimSpace(file.FileName))
	if name == "" || name == "." || name == string(filepath.Separator) {
		name = "launcher"
	}

	dst := filepath.Join(dir, name)
	out, err := os.Create(dst)
	if err != nil {
		return models.LauncherReleaseFile{}, err
	}

	hasher := sha256.New()
	size, copyErr := io.Copy(io.MultiWriter(out, hasher), io.LimitReader(file.Reader, maxReleaseFileSize+1))
	closeErr := out.Close()
	if copyErr != nil || closeErr != nil {
		_ = os.Remove(dst)
		return models.LauncherReleaseFile{}, errors.New("не удалось сохранить файл релиза")
	}
	if size > maxReleaseFileSize {
		_ = os.Remove(dst)
		return models.LauncherReleaseFile{}, errors.New("файл релиза превышает 200 МБ")
	}
	if size == 0 {
		_ = os.Remove(dst)
		return models.LauncherReleaseFile{}, errors.New("файл релиза пуст")
	}

	return models.LauncherReleaseFile{
		ID:         uuid.NewString(),
		ReleaseID:  releaseID,
		Platform:   file.Platform,
		FileName:   name,
		HashSHA256: hex.EncodeToString(hasher.Sum(nil)),
		Size:       size,
	}, nil
}

func (s Service) List(ctx context.Context) ([]models.LauncherRelease, error) {
	releases := make([]models.LauncherRelease, 0)
	err := s.db.WithContext(ctx).Preload("Files").
		Order("created_at DESC").Find(&releases).Error
	return releases, err
}

func (s Service) Update(ctx context.Context, id string, req PatchRequest) (models.LauncherRelease, error) {
	var release models.LauncherRelease
	if err := s.db.WithContext(ctx).Preload("Files").First(&release, "id = ?", id).Error; err != nil {
		return models.LauncherRelease{}, err
	}
	if req.Mandatory != nil {
		release.Mandatory = *req.Mandatory
	}
	if req.IsActive != nil {
		release.IsActive = *req.IsActive
	}
	if err := s.db.WithContext(ctx).Model(&models.LauncherRelease{ID: release.ID}).
		Updates(map[string]any{"mandatory": release.Mandatory, "is_active": release.IsActive}).Error; err != nil {
		return models.LauncherRelease{}, err
	}
	return release, nil
}

func (s Service) Delete(ctx context.Context, id string) error {
	var release models.LauncherRelease
	if err := s.db.WithContext(ctx).First(&release, "id = ?", id).Error; err != nil {
		return err
	}
	if err := s.db.WithContext(ctx).
		Where("release_id = ?", release.ID).Delete(&models.LauncherReleaseFile{}).Error; err != nil {
		return err
	}
	if err := s.db.WithContext(ctx).Delete(&release).Error; err != nil {
		return err
	}
	return os.RemoveAll(filepath.Join(s.storageRoot, release.Version))
}

// CheckUpdate — есть ли обновление для клиента version на платформе platform.
// Mandatory=true, если в интервале (version, latest] есть активный mandatory-релиз.
func (s Service) CheckUpdate(ctx context.Context, platform, clientVersion string) (UpdateInfo, error) {
	if !isAllowedPlatform(platform) {
		return UpdateInfo{}, errors.New("неизвестная платформа")
	}
	var releases []models.LauncherRelease
	if err := s.db.WithContext(ctx).Preload("Files").
		Where("is_active = ?", true).Find(&releases).Error; err != nil {
		return UpdateInfo{}, err
	}

	// Самый новый активный релиз, у которого есть бинарник под платформу.
	var latest *models.LauncherRelease
	var latestFile models.LauncherReleaseFile
	for i := range releases {
		for _, file := range releases[i].Files {
			if file.Platform != platform {
				continue
			}
			if latest == nil || CompareVersions(releases[i].Version, latest.Version) > 0 {
				latest = &releases[i]
				latestFile = file
			}
		}
	}
	if latest == nil || CompareVersions(latest.Version, clientVersion) <= 0 {
		return UpdateInfo{UpdateAvailable: false, LatestVersion: clientVersion}, nil
	}

	mandatory := false
	for _, release := range releases {
		if release.Mandatory &&
			CompareVersions(release.Version, clientVersion) > 0 &&
			CompareVersions(release.Version, latest.Version) <= 0 {
			mandatory = true
			break
		}
	}

	return UpdateInfo{
		UpdateAvailable: true,
		LatestVersion:   latest.Version,
		Mandatory:       mandatory,
		Changelog:       latest.Changelog,
		DownloadURL:     "/api/launcher/download/" + latest.Version + "/" + platform,
		SHA256:          latestFile.HashSHA256,
		Size:            latestFile.Size,
	}, nil
}

// MinMandatoryVersion — максимальная версия среди активных обязательных
// релизов; клиенты ниже неё не получают launch-token (426 в anticheat).
// Пустая строка — обязательных релизов нет.
func (s Service) MinMandatoryVersion(ctx context.Context) (string, error) {
	var releases []models.LauncherRelease
	if err := s.db.WithContext(ctx).
		Where("is_active = ? AND mandatory = ?", true, true).Find(&releases).Error; err != nil {
		return "", err
	}
	max := ""
	for _, release := range releases {
		if max == "" || CompareVersions(release.Version, max) > 0 {
			max = release.Version
		}
	}
	return max, nil
}

// LatestFile — самый новый активный релиз с бинарником под платформу.
// Возвращает релиз, файл и абсолютный путь к бинарнику. Если активных релизов
// под платформу нет — gorm.ErrRecordNotFound. Используется ботом для раздачи
// последнего релиза по кнопке «Скачать лаунчер».
func (s Service) LatestFile(ctx context.Context, platform string) (models.LauncherRelease, models.LauncherReleaseFile, string, error) {
	if !isAllowedPlatform(platform) {
		return models.LauncherRelease{}, models.LauncherReleaseFile{}, "", errors.New("неизвестная платформа")
	}
	var releases []models.LauncherRelease
	if err := s.db.WithContext(ctx).Preload("Files").
		Where("is_active = ?", true).Find(&releases).Error; err != nil {
		return models.LauncherRelease{}, models.LauncherReleaseFile{}, "", err
	}
	var latest *models.LauncherRelease
	var latestFile models.LauncherReleaseFile
	for i := range releases {
		for _, file := range releases[i].Files {
			if file.Platform != platform {
				continue
			}
			if latest == nil || CompareVersions(releases[i].Version, latest.Version) > 0 {
				latest = &releases[i]
				latestFile = file
			}
		}
	}
	if latest == nil {
		return models.LauncherRelease{}, models.LauncherReleaseFile{}, "", gorm.ErrRecordNotFound
	}
	abs, err := filepath.Abs(filepath.Join(s.storageRoot, latest.Version, platform, latestFile.FileName))
	if err != nil {
		return models.LauncherRelease{}, models.LauncherReleaseFile{}, "", err
	}
	return *latest, latestFile, abs, nil
}

// Download — абсолютный путь к бинарнику активного релиза. Версия проходит
// строгую валидацию (только цифры и точки), платформа — allowlist, имя файла
// берётся из БД (сохранялось через filepath.Base) — traversal невозможен.
func (s Service) Download(ctx context.Context, version, platform string) (string, models.LauncherReleaseFile, error) {
	if !ValidVersion(version) || !isAllowedPlatform(platform) {
		return "", models.LauncherReleaseFile{}, errors.New("некорректный запрос")
	}
	var release models.LauncherRelease
	if err := s.db.WithContext(ctx).
		Where("version = ? AND is_active = ?", version, true).First(&release).Error; err != nil {
		return "", models.LauncherReleaseFile{}, err
	}
	var file models.LauncherReleaseFile
	if err := s.db.WithContext(ctx).
		Where("release_id = ? AND platform = ?", release.ID, platform).First(&file).Error; err != nil {
		return "", models.LauncherReleaseFile{}, err
	}
	abs, err := filepath.Abs(filepath.Join(s.storageRoot, version, platform, file.FileName))
	if err != nil {
		return "", models.LauncherReleaseFile{}, err
	}
	return abs, file, nil
}
