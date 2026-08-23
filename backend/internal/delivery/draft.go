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
	var profile models.Profile
	if err := s.db.WithContext(ctx).First(&profile, "id = ?", profileID).Error; err != nil {
		s.publishMu.Unlock()
		return SeededProfileDraft{}, err
	}
	var release models.ProfileRelease
	if err := s.db.WithContext(ctx).
		Where("profile_id = ? AND is_active = ?", profileID, true).
		First(&release).Error; err != nil {
		s.publishMu.Unlock()
		return SeededProfileDraft{}, err
	}
	generation := time.Now().UTC().Format("20060102T150405Z") + "-" + newID()
	started := time.Now().UTC()
	job := models.DeliveryJob{
		ID: newID(), Kind: "profile", ProfileID: &profileID, Generation: generation,
		Status: "running", Phase: "seeding", Message: "Материализуем активный release из CAS", Progress: 0,
		SourceReleaseID: &release.ID, StartedAt: &started,
	}
	if err := s.db.WithContext(ctx).Create(&job).Error; err != nil {
		s.publishMu.Unlock()
		return SeededProfileDraft{}, err
	}
	// The source release is now pinned by the durable job, so CAS materialization
	// does not need to block unrelated publication or explicit GC.
	s.publishMu.Unlock()
	return s.materializeSeedDraft(ctx, &job)
}

func (s *Service) materializeSeedDraft(ctx context.Context, job *models.DeliveryJob) (SeededProfileDraft, error) {
	if job.ProfileID == nil || job.SourceReleaseID == nil {
		return SeededProfileDraft{}, errors.New("seed job is missing profile or source release")
	}
	profileID := *job.ProfileID
	sourceReleaseID := *job.SourceReleaseID
	var profile models.Profile
	if err := s.db.WithContext(ctx).First(&profile, "id = ?", profileID).Error; err != nil {
		return s.failSeedDraft(job, err)
	}
	var release models.ProfileRelease
	if err := s.db.WithContext(ctx).
		Preload("Files", func(db *gorm.DB) *gorm.DB { return db.Order("path ASC") }).
		Preload("Files.Chunks", func(db *gorm.DB) *gorm.DB { return db.Order("ordinal ASC") }).
		Where("id = ? AND profile_id = ?", sourceReleaseID, profileID).
		First(&release).Error; err != nil {
		return s.failSeedDraft(job, err)
	}
	preservePaths := effectivePreservePaths(profile.PreservePaths)
	root := filepath.Join(s.incomingRoot(), profileID)
	path := filepath.Join(root, job.Generation+".upload")
	seedingPath := filepath.Join(root, job.Generation+".seeding")
	if err := os.RemoveAll(path); err != nil {
		return s.failSeedDraft(job, err)
	}
	if err := os.RemoveAll(seedingPath); err != nil {
		return s.failSeedDraft(job, err)
	}
	if err := os.MkdirAll(seedingPath, 0755); err != nil {
		return s.failSeedDraft(job, err)
	}
	result := SeededProfileDraft{Generation: job.Generation, Path: path, SourceReleaseID: sourceReleaseID}
	for _, file := range release.Files {
		if pathMatchesPreserve(file.Path, preservePaths) {
			continue
		}
		if err := s.materializeProfileReleaseFile(seedingPath, file); err != nil {
			return s.failSeedDraft(job, fmt.Errorf("seed draft %s: %w", file.Path, err))
		}
		result.SeededFiles++
		result.SeededSize += file.Size
	}
	for _, suffix := range []string{".ready", ".processing"} {
		if _, err := os.Lstat(filepath.Join(root, job.Generation+suffix)); err == nil {
			return s.failSeedDraft(job, fmt.Errorf("generation %s was completed before seeding finished", job.Generation))
		} else if !os.IsNotExist(err) {
			return s.failSeedDraft(job, err)
		}
	}
	if err := os.Rename(seedingPath, path); err != nil {
		return s.failSeedDraft(job, err)
	}
	updates := map[string]any{
		"status": "waiting", "phase": "upload", "message": "Черновик заполнен из активного release; ожидаем atomic rename .upload -> .ready",
		"progress": 0.0, "error": "", "source_release_id": nil,
	}
	if err := s.db.WithContext(ctx).Model(&models.DeliveryJob{}).Where("id = ?", job.ID).Updates(updates).Error; err != nil {
		return s.failSeedDraft(job, err)
	}
	job.Status = "waiting"
	job.Phase = "upload"
	job.SourceReleaseID = nil
	return result, nil
}

func (s *Service) failSeedDraft(job *models.DeliveryJob, seedErr error) (SeededProfileDraft, error) {
	if job.ProfileID != nil {
		root := filepath.Join(s.incomingRoot(), *job.ProfileID)
		_ = os.RemoveAll(filepath.Join(root, job.Generation+".upload"))
		_ = os.RemoveAll(filepath.Join(root, job.Generation+".seeding"))
	}
	ended := time.Now().UTC()
	_ = s.db.WithContext(context.Background()).Model(&models.DeliveryJob{}).Where("id = ?", job.ID).Updates(map[string]any{
		"status": "failed", "phase": "failed", "message": "Создание черновика отклонено", "error": seedErr.Error(), "ended_at": &ended,
	}).Error
	return SeededProfileDraft{}, seedErr
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
	managedKey := strings.ToLower(strings.TrimSuffix(managedPath, "/"))
	for _, preservePath := range preservePaths {
		preserveKey := strings.ToLower(strings.TrimSuffix(preservePath, "/"))
		if preserveKey != "" && (managedKey == preserveKey || strings.HasPrefix(managedKey, preserveKey+"/") || strings.HasPrefix(preserveKey, managedKey+"/")) {
			return true
		}
	}
	return false
}
