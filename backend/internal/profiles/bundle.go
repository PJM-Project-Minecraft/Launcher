package profiles

import (
	"archive/tar"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"launcher-backend/internal/models"

	"github.com/klauspost/compress/zstd"
	"gorm.io/gorm"
)

type BundleDownload struct {
	AbsolutePath string
	ETag         string
	Size         int64
}

type createdBundle struct {
	AbsolutePath string
	HashSHA256   string
	Size         int64
}

func bundleInfo(profile models.Profile) *BundleInfo {
	if profile.ManifestVersion <= 0 || profile.BundleHashSHA256 == "" || profile.BundleSize <= 0 {
		return nil
	}
	return &BundleInfo{
		BuildID:     profile.ManifestVersion,
		Format:      "tar.zst",
		DownloadURL: fmt.Sprintf("/api/profiles/%s/bundles/%d", profile.ID, profile.ManifestVersion),
		HashSHA256:  profile.BundleHashSHA256,
		Size:        profile.BundleSize,
	}
}

func (s Service) bundlesRoot(profile models.Profile) string {
	return filepath.Join(s.publishedProfileRoot(profile.ID), "bundles")
}

func (s Service) bundlePath(profile models.Profile, version int) string {
	return filepath.Join(s.bundlesRoot(profile), fmt.Sprintf("%d.tar.zst", version))
}

// createBundle создаёт детерминированный архив опубликованного manifest. Во время
// упаковки каждый файл повторно хешируется: изменение staging между Scan и записью
// архива отменяет публикацию, а не создаёт manifest с чужими байтами.
func (s Service) createBundle(
	profile models.Profile,
	version int,
	files []models.GameFile,
	report func(written int64, path string),
) (createdBundle, error) {
	dir := s.bundlesRoot(profile)
	if err := os.MkdirAll(dir, 0755); err != nil {
		return createdBundle{}, err
	}
	finalPath := s.bundlePath(profile, version)
	temp, err := os.CreateTemp(dir, ".bundle-*.part")
	if err != nil {
		return createdBundle{}, err
	}
	tempPath := temp.Name()
	ok := false
	defer func() {
		_ = temp.Close()
		if !ok {
			_ = os.Remove(tempPath)
		}
	}()

	archiveHash := sha256.New()
	encoder, err := zstd.NewWriter(io.MultiWriter(temp, archiveHash),
		zstd.WithEncoderLevel(zstd.SpeedFastest),
		zstd.WithEncoderConcurrency(1),
	)
	if err != nil {
		return createdBundle{}, err
	}
	tarWriter := tar.NewWriter(encoder)

	var totalWritten int64
	step := progressStep(len(files), 100)
	for index, file := range files {
		rel, err := safeRelativePath(file.Path)
		if err != nil {
			return createdBundle{}, err
		}
		sourcePath, err := safeJoin(s.filesRoot(profile), rel)
		if err != nil {
			return createdBundle{}, err
		}
		source, err := os.Open(sourcePath)
		if err != nil {
			return createdBundle{}, fmt.Errorf("bundle source %s: %w", rel, err)
		}
		header := &tar.Header{
			Name:       filepath.ToSlash(rel),
			Mode:       bundleFileMode(file),
			Size:       file.Size,
			ModTime:    time.Unix(0, 0).UTC(),
			AccessTime: time.Unix(0, 0).UTC(),
			ChangeTime: time.Unix(0, 0).UTC(),
			Typeflag:   tar.TypeReg,
			Format:     tar.FormatPAX,
		}
		if err := tarWriter.WriteHeader(header); err != nil {
			source.Close()
			return createdBundle{}, err
		}
		fileHash := sha256.New()
		written, copyErr := io.Copy(io.MultiWriter(tarWriter, fileHash), source)
		closeErr := source.Close()
		if copyErr != nil || closeErr != nil {
			return createdBundle{}, fmt.Errorf("bundle read %s failed", rel)
		}
		if written != file.Size || hex.EncodeToString(fileHash.Sum(nil)) != strings.ToLower(file.HashSHA256) {
			return createdBundle{}, fmt.Errorf("файл %s изменился во время публикации; повторите", rel)
		}
		totalWritten += written
		if report != nil && (index == 0 || (index+1)%step == 0 || index+1 == len(files)) {
			report(totalWritten, rel)
		}
	}
	if err := tarWriter.Close(); err != nil {
		return createdBundle{}, err
	}
	if err := encoder.Close(); err != nil {
		return createdBundle{}, err
	}
	if err := temp.Sync(); err != nil {
		return createdBundle{}, err
	}
	if err := temp.Close(); err != nil {
		return createdBundle{}, err
	}
	info, err := os.Stat(tempPath)
	if err != nil {
		return createdBundle{}, err
	}
	if err := os.Rename(tempPath, finalPath); err != nil {
		return createdBundle{}, err
	}
	ok = true
	return createdBundle{AbsolutePath: finalPath, HashSHA256: hex.EncodeToString(archiveHash.Sum(nil)), Size: info.Size()}, nil
}

func bundleFileMode(file models.GameFile) int64 {
	if file.Executable {
		return 0755
	}
	return 0644
}

func (s Service) Bundle(ctx context.Context, profileID string, version int) (BundleDownload, error) {
	var profile models.Profile
	if err := s.db.WithContext(ctx).Where("id = ? AND is_active = ?", profileID, true).First(&profile).Error; err != nil {
		return BundleDownload{}, err
	}
	if version <= 0 || version > profile.ManifestVersion {
		return BundleDownload{}, gorm.ErrRecordNotFound
	}
	path := s.bundlePath(profile, version)
	info, err := os.Stat(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return BundleDownload{}, gorm.ErrRecordNotFound
		}
		return BundleDownload{}, err
	}
	if !info.Mode().IsRegular() {
		return BundleDownload{}, gorm.ErrRecordNotFound
	}
	return BundleDownload{
		AbsolutePath: path,
		ETag:         fmt.Sprintf("profile-%s-v%d-%d", profile.ID, version, info.Size()),
		Size:         info.Size(),
	}, nil
}

func (s Service) cleanupOldBundles(profile models.Profile, keep int) {
	entries, err := os.ReadDir(s.bundlesRoot(profile))
	if err != nil {
		return
	}
	type versionedPath struct {
		version int
		path    string
	}
	var bundles []versionedPath
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".tar.zst") {
			continue
		}
		version, err := strconv.Atoi(strings.TrimSuffix(entry.Name(), ".tar.zst"))
		if err == nil {
			bundles = append(bundles, versionedPath{version: version, path: filepath.Join(s.bundlesRoot(profile), entry.Name())})
		}
	}
	sort.Slice(bundles, func(i, j int) bool { return bundles[i].version > bundles[j].version })
	if len(bundles) <= keep {
		return
	}
	for _, bundle := range bundles[keep:] {
		_ = os.Remove(bundle.path)
	}
}
