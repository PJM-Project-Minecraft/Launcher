package delivery

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
	"time"

	"launcher-backend/internal/models"

	"gorm.io/gorm"
)

type SeededProfileDraft struct {
	Generation      string
	Path            string
	SourceReleaseID string
	SeededFiles     int
	SeededSize      int64
}

func (s *Service) CreateDraftFromActive(ctx context.Context, profileID string) (SeededProfileDraft, error) {
	s.publishMu.Lock()
	defer s.publishMu.Unlock()

	var profile models.Profile
	if err := s.db.WithContext(ctx).First(&profile, "id = ?", profileID).Error; err != nil {
		return SeededProfileDraft{}, err
	}
	var release models.ProfileRelease
	if err := s.db.WithContext(ctx).
		Preload("Files", func(db *gorm.DB) *gorm.DB { return db.Order("path ASC") }).
		Preload("Files.Chunks", func(db *gorm.DB) *gorm.DB { return db.Order("ordinal ASC") }).
		Where("profile_id = ? AND is_active = ?", profileID, true).
		First(&release).Error; err != nil {
		return SeededProfileDraft{}, err
	}

	preservePaths := append([]string(nil), profile.PreservePaths...)
	if len(preservePaths) == 0 {
		preservePaths = append([]string(nil), defaultPreservePaths...)
	}
	generation := time.Now().UTC().Format("20060102T150405Z") + "-" + newID()
	path := filepath.Join(s.incomingRoot(), profileID, generation+".upload")
	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		return SeededProfileDraft{}, err
	}
	if err := os.Mkdir(path, 0755); err != nil {
		return SeededProfileDraft{}, err
	}
	keep := false
	defer func() {
		if !keep {
			_ = os.RemoveAll(path)
		}
	}()

	result := SeededProfileDraft{Generation: generation, Path: path, SourceReleaseID: release.ID}
	for _, file := range release.Files {
		if pathMatchesPreserve(file.Path, preservePaths) {
			continue
		}
		if err := s.materializeProfileReleaseFile(path, file); err != nil {
			return SeededProfileDraft{}, fmt.Errorf("seed draft %s: %w", file.Path, err)
		}
		result.SeededFiles++
		result.SeededSize += file.Size
	}
	job := models.DeliveryJob{
		ID: newID(), Kind: "profile", ProfileID: &profileID, Generation: generation,
		Status: "waiting", Phase: "upload", Message: "Черновик заполнен из активного release; ожидаем atomic rename .upload -> .ready",
	}
	if err := s.db.WithContext(ctx).Create(&job).Error; err != nil {
		return SeededProfileDraft{}, err
	}
	keep = true
	return result, nil
}

func (s *Service) materializeProfileReleaseFile(root string, file models.ProfileReleaseFile) error {
	target := filepath.Join(root, filepath.FromSlash(file.Path))
	relative, err := safeRelative(root, target)
	if err != nil || relative != file.Path {
		return errors.New("stored release contains an unsafe path")
	}
	if err := os.MkdirAll(filepath.Dir(target), 0755); err != nil {
		return err
	}
	temp := target + ".seed-part"
	output, err := os.OpenFile(temp, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		return err
	}
	ok := false
	defer func() {
		_ = output.Close()
		if !ok {
			_ = os.Remove(temp)
		}
	}()
	whole := sha256.New()
	var written int64
	for _, chunk := range file.Chunks {
		metadata, err := os.Lstat(s.blobPath(chunk.HashSHA256))
		if err != nil || !metadata.Mode().IsRegular() || metadata.Mode()&os.ModeSymlink != 0 || metadata.Size() != chunk.Size {
			return fmt.Errorf("CAS chunk %s is missing or invalid", chunk.HashSHA256)
		}
		input, err := os.Open(s.blobPath(chunk.HashSHA256))
		if err != nil {
			return err
		}
		chunkHash := sha256.New()
		copied, copyErr := io.Copy(io.MultiWriter(output, whole, chunkHash), input)
		closeErr := input.Close()
		if copyErr != nil {
			return copyErr
		}
		if closeErr != nil {
			return closeErr
		}
		if copied != chunk.Size || hex.EncodeToString(chunkHash.Sum(nil)) != chunk.HashSHA256 {
			return fmt.Errorf("CAS chunk %s failed verification", chunk.HashSHA256)
		}
		written += copied
	}
	if written != file.Size || hex.EncodeToString(whole.Sum(nil)) != file.HashSHA256 {
		return errors.New("materialized file failed verification")
	}
	if err := output.Close(); err != nil {
		return err
	}
	mode := os.FileMode(0644)
	if file.Executable {
		mode = 0755
	}
	if err := os.Chmod(temp, mode); err != nil {
		return err
	}
	if err := os.Rename(temp, target); err != nil {
		return err
	}
	ok = true
	return nil
}

func pathMatchesPreserve(managedPath string, preservePaths []string) bool {
	for _, preservePath := range preservePaths {
		if strings.HasSuffix(preservePath, "/") {
			if managedPath == strings.TrimSuffix(preservePath, "/") || strings.HasPrefix(managedPath, preservePath) {
				return true
			}
			continue
		}
		if managedPath == preservePath {
			return true
		}
	}
	return false
}
