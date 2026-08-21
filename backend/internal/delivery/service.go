package delivery

import (
	"context"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"

	"launcher-backend/internal/models"

	"github.com/google/uuid"
	"gorm.io/gorm"
)

type Service struct {
	db           *gorm.DB
	root         string
	profileRoot  string
	launcherRoot string
	signingKey   ed25519.PrivateKey
	publishMu    sync.Mutex
	stop         chan struct{}
}

func NewService(db *gorm.DB, root, profileRoot, launcherRoot, signingSeedHex string) (*Service, error) {
	service := &Service{db: db, root: root, profileRoot: profileRoot, launcherRoot: launcherRoot, stop: make(chan struct{})}
	if signingSeedHex != "" {
		seed, err := hex.DecodeString(signingSeedHex)
		if err != nil || len(seed) != ed25519.SeedSize {
			return nil, errors.New("DELIVERY_MANIFEST_SIGNING_KEY must be a 64-character Ed25519 seed")
		}
		service.signingKey = ed25519.NewKeyFromSeed(seed)
	}
	for _, path := range []string{service.blobsRoot(), service.manifestsRoot(), service.incomingRoot()} {
		if err := os.MkdirAll(path, 0755); err != nil {
			return nil, err
		}
	}
	return service, nil
}

func (s *Service) blobsRoot() string     { return filepath.Join(s.root, "blobs") }
func (s *Service) manifestsRoot() string { return filepath.Join(s.root, "manifests") }
func (s *Service) incomingRoot() string  { return filepath.Join(s.root, "incoming", "profiles") }
func (s *Service) blobPath(hash string) string {
	return filepath.Join(s.blobsRoot(), hash[:2], hash)
}
func (s *Service) manifestPath(releaseID string) string {
	return filepath.Join(s.manifestsRoot(), releaseID+".json")
}

func (s *Service) SigningPublicKeyHex() string {
	if len(s.signingKey) == 0 {
		return ""
	}
	return hex.EncodeToString(s.signingKey.Public().(ed25519.PublicKey))
}

func (s *Service) SignManifest(data []byte) string {
	if len(s.signingKey) == 0 {
		return ""
	}
	return hex.EncodeToString(ed25519.Sign(s.signingKey, data))
}

func (s *Service) BackfillProfile(ctx context.Context, profileID, source string) (string, error) {
	var current models.ProfileRelease
	if err := s.db.WithContext(ctx).Where("profile_id = ? AND is_active = ?", profileID, true).First(&current).Error; err == nil {
		return current.ID, nil
	} else if !errors.Is(err, gorm.ErrRecordNotFound) {
		return "", err
	}
	s.publishMu.Lock()
	defer s.publishMu.Unlock()
	return s.publishProfile(ctx, profileID, source, func(_, _ int) {})
}

func validHash(value string) bool {
	if len(value) != 64 {
		return false
	}
	_, err := hex.DecodeString(value)
	return err == nil && strings.ToLower(value) == value
}

func (s *Service) storeBlob(ctx context.Context, chunk chunkData) error {
	if !validHash(chunk.Hash) {
		return errors.New("invalid chunk digest")
	}
	target := s.blobPath(chunk.Hash)
	if storedBlobValid(target, chunk.Hash, int64(len(chunk.Data))) {
		return s.db.WithContext(ctx).FirstOrCreate(&models.DeliveryBlob{HashSHA256: chunk.Hash, Size: int64(len(chunk.Data))}).Error
	}
	_ = os.Remove(target)
	if err := os.MkdirAll(filepath.Dir(target), 0755); err != nil {
		return err
	}
	temp, err := os.CreateTemp(filepath.Dir(target), ".blob-*.part")
	if err != nil {
		return err
	}
	tempPath := temp.Name()
	ok := false
	defer func() {
		_ = temp.Close()
		if !ok {
			_ = os.Remove(tempPath)
		}
	}()
	if _, err := temp.Write(chunk.Data); err != nil {
		return err
	}
	if err := temp.Sync(); err != nil {
		return err
	}
	if err := temp.Close(); err != nil {
		return err
	}
	if err := os.Rename(tempPath, target); err != nil {
		if info, statErr := os.Stat(target); statErr != nil || info.Size() != int64(len(chunk.Data)) {
			return err
		}
	}
	ok = true
	return s.db.WithContext(ctx).FirstOrCreate(&models.DeliveryBlob{HashSHA256: chunk.Hash, Size: int64(len(chunk.Data))}).Error
}

func storedBlobValid(path, expectedHash string, expectedSize int64) bool {
	info, err := os.Lstat(path)
	if err != nil || !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 || info.Size() != expectedSize {
		return false
	}
	file, err := os.Open(path)
	if err != nil {
		return false
	}
	defer file.Close()
	hash := sha256.New()
	if _, err := io.Copy(hash, file); err != nil {
		return false
	}
	return hex.EncodeToString(hash.Sum(nil)) == expectedHash
}

func (s *Service) chunkFile(ctx context.Context, path string) (ReleaseFile, error) {
	before, err := os.Lstat(path)
	if err != nil {
		return ReleaseFile{}, err
	}
	if !before.Mode().IsRegular() {
		return ReleaseFile{}, errors.New("delivery source contains a non-regular file")
	}
	file, err := os.Open(path)
	if err != nil {
		return ReleaseFile{}, err
	}
	defer file.Close()
	whole := sha256.New()
	refs := make([]ChunkRef, 0)
	err = splitChunks(io.TeeReader(file, whole), func(chunk chunkData) error {
		if err := s.storeBlob(ctx, chunk); err != nil {
			return err
		}
		refs = append(refs, ChunkRef{SHA256: chunk.Hash, Size: int64(len(chunk.Data))})
		return nil
	})
	if err != nil {
		return ReleaseFile{}, err
	}
	after, err := os.Lstat(path)
	if err != nil || before.Size() != after.Size() || !before.ModTime().Equal(after.ModTime()) {
		return ReleaseFile{}, errors.New("source changed during publication")
	}
	return ReleaseFile{
		Size: before.Size(), SHA256: hex.EncodeToString(whole.Sum(nil)),
		Executable: before.Mode().Perm()&0111 != 0, Chunks: refs,
	}, nil
}

func safeRelative(root, path string) (string, error) {
	relative, err := filepath.Rel(root, path)
	if err != nil {
		return "", err
	}
	relative = filepath.ToSlash(relative)
	if relative == "." || relative == "" || strings.HasPrefix(relative, "../") || filepath.IsAbs(relative) {
		return "", errors.New("unsafe delivery path")
	}
	for _, part := range strings.Split(relative, "/") {
		if part == "" || part == "." || part == ".." || strings.TrimRight(part, ". ") != part || strings.ContainsAny(part, `<>:"\|?*`) {
			return "", fmt.Errorf("path is not portable: %s", relative)
		}
	}
	return relative, nil
}

func (s *Service) collectFiles(ctx context.Context, root string, progress func(int, int)) ([]ReleaseFile, int64, error) {
	paths := make([]string, 0)
	err := filepath.WalkDir(root, func(path string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if path == root || entry.IsDir() {
			return nil
		}
		if entry.Type()&os.ModeSymlink != 0 {
			return fmt.Errorf("symlink is forbidden: %s", path)
		}
		paths = append(paths, path)
		return nil
	})
	if err != nil {
		return nil, 0, err
	}
	sort.Strings(paths)
	seen := make(map[string]string, len(paths))
	files := make([]ReleaseFile, 0, len(paths))
	var total int64
	for index, path := range paths {
		relative, err := safeRelative(root, path)
		if err != nil {
			return nil, 0, err
		}
		folded := strings.ToLower(relative)
		if previous, exists := seen[folded]; exists {
			return nil, 0, fmt.Errorf("case-conflicting paths: %s and %s", previous, relative)
		}
		seen[folded] = relative
		file, err := s.chunkFile(ctx, path)
		if err != nil {
			return nil, 0, fmt.Errorf("%s: %w", relative, err)
		}
		file.Path = relative
		files = append(files, file)
		total += file.Size
		progress(index+1, len(paths))
	}
	return files, total, nil
}

func writeAtomic(path string, data []byte) error {
	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		return err
	}
	temp, err := os.CreateTemp(filepath.Dir(path), ".manifest-*.part")
	if err != nil {
		return err
	}
	name := temp.Name()
	ok := false
	defer func() {
		_ = temp.Close()
		if !ok {
			_ = os.Remove(name)
		}
	}()
	if _, err := temp.Write(data); err != nil {
		return err
	}
	if err := temp.Sync(); err != nil {
		return err
	}
	if err := temp.Close(); err != nil {
		return err
	}
	if err := os.Rename(name, path); err != nil {
		return err
	}
	ok = true
	return nil
}

func (s *Service) profileConfig(profile models.Profile) ProfileConfig {
	return ProfileConfig{
		ID: profile.ID, Name: profile.Name, GameVersion: profile.GameVersion,
		Loader: profile.Loader, LoaderVersion: profile.LoaderVersion, JavaVersion: profile.JavaVersion,
		JVMArgs: profile.JVMArgs, JavaPathWindows: profile.JavaPathWindows, JavaPathLinux: profile.JavaPathLinux,
		JavaPathMacOS: profile.JavaPathMacOS, LaunchCommandWindows: profile.LaunchCommandWindows,
		LaunchCommandLinux: profile.LaunchCommandLinux, LaunchCommandMacOS: profile.LaunchCommandMacOS,
		PreservePaths: append([]string(nil), profile.PreservePaths...),
	}
}

func (s *Service) Profiles(ctx context.Context) ([]ProfileSummary, error) {
	var profiles []models.Profile
	if err := s.db.WithContext(ctx).Where("is_active = ?", true).Order("created_at asc").Find(&profiles).Error; err != nil {
		return nil, err
	}
	result := make([]ProfileSummary, 0, len(profiles))
	for _, profile := range profiles {
		var release models.ProfileRelease
		err := s.db.WithContext(ctx).Where("profile_id = ? AND is_active = ?", profile.ID, true).First(&release).Error
		if err != nil && !errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, err
		}
		item := ProfileSummary{ID: profile.ID, Name: profile.Name, Description: profile.Description, GameVersion: profile.GameVersion, Loader: profile.Loader, IconURL: profile.IconURL, IsActive: profile.IsActive}
		if err == nil {
			item.ActiveReleaseID = release.ID
			item.ManifestSHA256 = release.ManifestSHA256
			item.ManifestSignature = release.ManifestSignature
			item.FileCount = release.FileCount
			item.TotalSize = release.TotalSize
			created := release.CreatedAt
			item.ReleaseCreatedAt = &created
		}
		result = append(result, item)
	}
	return result, nil
}

func (s *Service) Manifest(ctx context.Context, profileID, releaseID string) ([]byte, models.ProfileRelease, error) {
	var release models.ProfileRelease
	if err := s.db.WithContext(ctx).Where("id = ? AND profile_id = ?", releaseID, profileID).First(&release).Error; err != nil {
		return nil, release, err
	}
	data, err := os.ReadFile(s.manifestPath(release.ID))
	if err != nil {
		return nil, release, err
	}
	digest := sha256.Sum256(data)
	if hex.EncodeToString(digest[:]) != release.ManifestSHA256 {
		return nil, release, errors.New("stored manifest digest mismatch")
	}
	return data, release, nil
}

func (s *Service) Blob(ctx context.Context, profileID, hash string) (string, int64, error) {
	if !validHash(hash) {
		return "", 0, gorm.ErrRecordNotFound
	}
	var count int64
	err := s.db.WithContext(ctx).Table("profile_release_file_chunks AS chunks").
		Joins("JOIN profile_release_files AS files ON files.id = chunks.file_id").
		Joins("JOIN profile_releases AS releases ON releases.id = files.release_id").
		Where("releases.profile_id = ? AND chunks.hash_sha256 = ?", profileID, hash).Count(&count).Error
	if err != nil || count == 0 {
		if err == nil {
			err = gorm.ErrRecordNotFound
		}
		return "", 0, err
	}
	var blob models.DeliveryBlob
	if err := s.db.WithContext(ctx).First(&blob, "hash_sha256 = ?", hash).Error; err != nil {
		return "", 0, err
	}
	return s.blobPath(hash), blob.Size, nil
}

func (s *Service) Jobs(ctx context.Context) ([]models.DeliveryJob, error) {
	var jobs []models.DeliveryJob
	err := s.db.WithContext(ctx).Order("created_at desc").Limit(100).Find(&jobs).Error
	return jobs, err
}

func parseVersion(value string) ([3]int, bool) {
	parts := strings.Split(value, ".")
	if len(parts) != 3 {
		return [3]int{}, false
	}
	var result [3]int
	for index, part := range parts {
		parsed, err := strconv.Atoi(part)
		if err != nil || parsed < 0 {
			return [3]int{}, false
		}
		result[index] = parsed
	}
	return result, true
}

func compareVersion(left, right string) int {
	a, _ := parseVersion(left)
	b, _ := parseVersion(right)
	for index := range a {
		if a[index] < b[index] {
			return -1
		}
		if a[index] > b[index] {
			return 1
		}
	}
	return 0
}

func (s *Service) LauncherCurrent(ctx context.Context, platform, from string) (*LauncherManifest, error) {
	if platform != "linux-x64" && platform != "windows-x64" {
		return nil, errors.New("unknown platform")
	}
	if _, ok := parseVersion(from); !ok {
		return nil, errors.New("from must use X.Y.Z format")
	}
	var releases []models.LauncherRelease
	if err := s.db.WithContext(ctx).Preload("Files").Where("is_active = ?", true).Find(&releases).Error; err != nil {
		return nil, err
	}
	sort.Slice(releases, func(i, j int) bool { return compareVersion(releases[i].Version, releases[j].Version) > 0 })
	var selected *models.LauncherRelease
	var selectedFile *models.LauncherReleaseFile
	for index := range releases {
		for fileIndex := range releases[index].Files {
			if releases[index].Files[fileIndex].Platform == platform {
				selected, selectedFile = &releases[index], &releases[index].Files[fileIndex]
				break
			}
		}
		if selected != nil {
			break
		}
	}
	if selected == nil || compareVersion(selected.Version, from) <= 0 {
		return nil, gorm.ErrRecordNotFound
	}
	var artifact models.LauncherDeliveryArtifact
	if err := s.db.WithContext(ctx).Preload("Chunks", func(db *gorm.DB) *gorm.DB { return db.Order("ordinal asc") }).Where("release_id = ? AND platform = ?", selected.ID, platform).First(&artifact).Error; err != nil {
		return nil, err
	}
	mandatory := false
	for _, release := range releases {
		if release.Mandatory && compareVersion(release.Version, from) > 0 && compareVersion(release.Version, selected.Version) <= 0 {
			mandatory = true
		}
	}
	chunks := make([]ChunkRef, 0, len(artifact.Chunks))
	for _, chunk := range artifact.Chunks {
		chunks = append(chunks, ChunkRef{SHA256: chunk.HashSHA256, Size: chunk.Size})
	}
	return &LauncherManifest{
		SchemaVersion: SchemaVersion, Kind: "launcher", ReleaseID: selected.ID,
		Version: selected.Version, Platform: platform, Mandatory: mandatory,
		Changelog: selected.Changelog, ArtifactSignature: selectedFile.SignatureEd25519,
		Artifact:    ReleaseFile{Path: selectedFile.FileName, Size: artifact.Size, SHA256: artifact.HashSHA256, Executable: true, Chunks: chunks},
		CreatedAt:   selected.CreatedAt,
		DownloadURL: "/api/v2/launcher/releases/" + selected.ID + "/artifact?platform=" + platform,
	}, nil
}

func (s *Service) LauncherArtifact(ctx context.Context, releaseID, platform string) (string, models.LauncherReleaseFile, error) {
	if platform != "linux-x64" && platform != "windows-x64" {
		return "", models.LauncherReleaseFile{}, gorm.ErrRecordNotFound
	}
	var release models.LauncherRelease
	if err := s.db.WithContext(ctx).Where("id = ? AND is_active = ?", releaseID, true).First(&release).Error; err != nil {
		return "", models.LauncherReleaseFile{}, err
	}
	var file models.LauncherReleaseFile
	if err := s.db.WithContext(ctx).Where("release_id = ? AND platform = ?", releaseID, platform).First(&file).Error; err != nil {
		return "", file, err
	}
	return filepath.Join(s.launcherRoot, release.Version, platform, file.FileName), file, nil
}

func (s *Service) LauncherBlob(ctx context.Context, releaseID, hash string) (string, int64, error) {
	if !validHash(hash) {
		return "", 0, gorm.ErrRecordNotFound
	}
	var count int64
	err := s.db.WithContext(ctx).Table("launcher_delivery_artifact_chunks AS chunks").
		Joins("JOIN launcher_delivery_artifacts AS artifacts ON artifacts.id = chunks.artifact_id").
		Joins("JOIN launcher_releases AS releases ON releases.id = artifacts.release_id").
		Where("releases.id = ? AND releases.is_active = ? AND chunks.hash_sha256 = ?", releaseID, true, hash).Count(&count).Error
	if err != nil || count == 0 {
		if err == nil {
			err = gorm.ErrRecordNotFound
		}
		return "", 0, err
	}
	var blob models.DeliveryBlob
	if err := s.db.WithContext(ctx).First(&blob, "hash_sha256 = ?", hash).Error; err != nil {
		return "", 0, err
	}
	return s.blobPath(hash), blob.Size, nil
}

func encodeManifest(manifest ProfileManifest) ([]byte, string, error) {
	data, err := json.Marshal(manifest)
	if err != nil {
		return nil, "", err
	}
	digest := sha256.Sum256(data)
	return data, hex.EncodeToString(digest[:]), nil
}

func newID() string { return uuid.NewString() }
