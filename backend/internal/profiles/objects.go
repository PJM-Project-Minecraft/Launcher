package profiles

import (
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

	"launcher-backend/internal/models"

	"gorm.io/gorm"
)

type ObjectDownload struct {
	AbsolutePath string
	ETag         string
	Size         int64
}

func (s Service) objectsRoot() string {
	return filepath.Join(s.storageRoot, ".objects")
}

func (s Service) objectPath(hash string) string {
	return filepath.Join(s.objectsRoot(), hash[:2], hash)
}

func (s Service) publishedProfileRoot(profileID string) string {
	return filepath.Join(s.storageRoot, ".published", profileID)
}

func (s Service) objectRefsRoot(profile models.Profile) string {
	return filepath.Join(s.publishedProfileRoot(profile.ID), "manifests")
}

func (s Service) objectRefsPath(profile models.Profile, version int) string {
	return filepath.Join(s.objectRefsRoot(profile), strconv.Itoa(version)+".sha256")
}

// ensureObject делает неизменяемую content-addressed копию файла staging.
// Малые обновления скачиваются отсюда и больше не зависят от последующих SFTP-правок.
func (s Service) ensureObject(profile models.Profile, file models.GameFile) error {
	hash := strings.ToLower(file.HashSHA256)
	if !validSHA256(hash) {
		return fmt.Errorf("invalid sha256 for %s", file.Path)
	}
	target := s.objectPath(hash)
	if info, err := os.Stat(target); err == nil && info.Mode().IsRegular() && info.Size() == file.Size {
		// Object создаётся атомарно только после SHA-проверки. Повторное чтение
		// каждого неизменяемого объекта здесь удваивало I/O всей публикации;
		// createBundle ниже всё равно проверяет SHA bytes из object-store.
		return nil
	}
	if err := os.MkdirAll(filepath.Dir(target), 0755); err != nil {
		return err
	}
	sourcePath, err := safeJoin(s.filesRoot(profile), file.Path)
	if err != nil {
		return err
	}
	source, err := os.Open(sourcePath)
	if err != nil {
		return err
	}
	defer source.Close()
	temp, err := os.CreateTemp(filepath.Dir(target), ".object-*.part")
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

	hasher := sha256.New()
	written, copyErr := io.Copy(io.MultiWriter(temp, hasher), source)
	closeErr := temp.Close()
	if copyErr != nil || closeErr != nil {
		return fmt.Errorf("failed to snapshot %s", file.Path)
	}
	if written != file.Size || hex.EncodeToString(hasher.Sum(nil)) != hash {
		return fmt.Errorf("файл %s изменился во время публикации; повторите", file.Path)
	}
	_ = os.Remove(target)
	if err := os.Rename(tempPath, target); err != nil {
		return err
	}
	ok = true
	return nil
}

func (s Service) Object(ctx context.Context, profileID, hash string) (ObjectDownload, error) {
	hash = strings.ToLower(strings.TrimSpace(hash))
	if !validSHA256(hash) {
		return ObjectDownload{}, gorm.ErrRecordNotFound
	}
	var profile models.Profile
	if err := s.db.WithContext(ctx).Where("id = ? AND is_active = ?", profileID, true).First(&profile).Error; err != nil {
		return ObjectDownload{}, err
	}
	// Retained manifest references keep an already issued manifest valid while
	// allowing garbage collection of objects no published build can use anymore.
	if !s.objectReferenced(profile, hash) {
		return ObjectDownload{}, gorm.ErrRecordNotFound
	}
	path := s.objectPath(hash)
	info, err := os.Stat(path)
	if err != nil || !info.Mode().IsRegular() {
		if errors.Is(err, os.ErrNotExist) || err == nil {
			return ObjectDownload{}, gorm.ErrRecordNotFound
		}
		return ObjectDownload{}, err
	}
	return ObjectDownload{AbsolutePath: path, ETag: hash, Size: info.Size()}, nil
}

func (s Service) writeObjectRefs(profile models.Profile, version int, files []models.GameFile) (string, error) {
	dir := s.objectRefsRoot(profile)
	if err := os.MkdirAll(dir, 0755); err != nil {
		return "", err
	}
	hashes := make([]string, 0, len(files))
	for _, file := range files {
		hashes = append(hashes, strings.ToLower(file.HashSHA256))
	}
	sort.Strings(hashes)
	temp, err := os.CreateTemp(dir, ".manifest-*.part")
	if err != nil {
		return "", err
	}
	tempPath := temp.Name()
	ok := false
	defer func() {
		_ = temp.Close()
		if !ok {
			_ = os.Remove(tempPath)
		}
	}()
	if _, err := io.WriteString(temp, strings.Join(hashes, "\n")+"\n"); err != nil {
		return "", err
	}
	if err := temp.Sync(); err != nil {
		return "", err
	}
	if err := temp.Close(); err != nil {
		return "", err
	}
	finalPath := s.objectRefsPath(profile, version)
	if err := os.Rename(tempPath, finalPath); err != nil {
		return "", err
	}
	ok = true
	return finalPath, nil
}

func (s Service) objectReferenced(profile models.Profile, hash string) bool {
	entries, err := os.ReadDir(s.objectRefsRoot(profile))
	if err != nil {
		return false
	}
	needle := []byte(hash)
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".sha256") {
			continue
		}
		contents, err := os.ReadFile(filepath.Join(s.objectRefsRoot(profile), entry.Name()))
		if err != nil {
			continue
		}
		for _, line := range strings.Fields(string(contents)) {
			if string(needle) == line {
				return true
			}
		}
	}
	return false
}

func (s Service) cleanupOldObjectRefs(profile models.Profile, keep int) {
	entries, err := os.ReadDir(s.objectRefsRoot(profile))
	if err != nil {
		return
	}
	type versionedPath struct {
		version int
		path    string
	}
	refs := make([]versionedPath, 0, len(entries))
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".sha256") {
			continue
		}
		version, err := strconv.Atoi(strings.TrimSuffix(entry.Name(), ".sha256"))
		if err == nil {
			refs = append(refs, versionedPath{version: version, path: filepath.Join(s.objectRefsRoot(profile), entry.Name())})
		}
	}
	sort.Slice(refs, func(i, j int) bool { return refs[i].version > refs[j].version })
	for _, ref := range refs[min(keep, len(refs)):] {
		_ = os.Remove(ref.path)
	}
}

// gcObjects removes snapshots no longer referenced by any of the retained
// manifests. It runs under scanMu, so two publications cannot race each other.
func (s Service) gcObjects() {
	live := make(map[string]struct{})
	publishedRoot := filepath.Join(s.storageRoot, ".published")
	_ = filepath.WalkDir(publishedRoot, func(path string, entry os.DirEntry, err error) error {
		if err != nil || entry.IsDir() || !strings.HasSuffix(entry.Name(), ".sha256") {
			return nil
		}
		contents, readErr := os.ReadFile(path)
		if readErr == nil {
			for _, hash := range strings.Fields(string(contents)) {
				if validSHA256(hash) {
					live[hash] = struct{}{}
				}
			}
		}
		return nil
	})
	_ = filepath.WalkDir(s.objectsRoot(), func(path string, entry os.DirEntry, err error) error {
		if err != nil || entry.IsDir() || !validSHA256(entry.Name()) {
			return nil
		}
		if _, retained := live[entry.Name()]; !retained {
			_ = os.Remove(path)
		}
		return nil
	})
}

func validSHA256(value string) bool {
	if len(value) != 64 {
		return false
	}
	for _, char := range value {
		if !((char >= '0' && char <= '9') || (char >= 'a' && char <= 'f')) {
			return false
		}
	}
	return true
}

func validatePortablePaths(files []models.GameFile) error {
	seen := make(map[string]string, len(files))
	for _, file := range files {
		path := filepath.ToSlash(file.Path)
		for _, component := range strings.Split(path, "/") {
			if !portablePathComponent(component) {
				return fmt.Errorf("путь несовместим с Windows/Linux: %s", path)
			}
		}
		folded := strings.ToLower(path)
		if previous, exists := seen[folded]; exists && previous != path {
			return fmt.Errorf("пути конфликтуют на Windows: %s и %s", previous, path)
		}
		seen[folded] = path
	}
	return nil
}

func portablePathComponent(value string) bool {
	if value == "" || strings.TrimRight(value, ". ") != value {
		return false
	}
	for _, char := range value {
		if char < 32 || strings.ContainsRune(`<>:"\|?*`, char) {
			return false
		}
	}
	base := strings.ToUpper(strings.SplitN(value, ".", 2)[0])
	if base == "CON" || base == "PRN" || base == "AUX" || base == "NUL" {
		return false
	}
	if len(base) == 4 && (strings.HasPrefix(base, "COM") || strings.HasPrefix(base, "LPT")) {
		return base[3] < '1' || base[3] > '9'
	}
	return true
}
