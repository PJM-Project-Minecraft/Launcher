package delivery

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"time"

	"launcher-backend/internal/models"

	"gorm.io/gorm"
)

const (
	gcFileSuffix     = ".gc"
	gcLauncherMarker = ".delivery-gc-"
)

// GCResult describes only records that were made unreachable by an explicit
// operator-run cleanup. Garbage collection is never started by the server.
type GCResult struct {
	ProfileReleases   int `json:"profileReleases"`
	LauncherReleases  int `json:"launcherReleases"`
	LauncherArtifacts int `json:"launcherArtifacts"`
	Blobs             int `json:"blobs"`
}

type gcMove struct {
	original   string
	quarantine string
	directory  bool
}

func stageGCPath(original, quarantine string, directory bool) (gcMove, bool, error) {
	if _, err := os.Stat(original); err != nil {
		if os.IsNotExist(err) {
			return gcMove{}, false, nil
		}
		return gcMove{}, false, err
	}
	if _, err := os.Stat(quarantine); err == nil {
		return gcMove{}, false, fmt.Errorf("GC quarantine already exists: %s", quarantine)
	} else if !os.IsNotExist(err) {
		return gcMove{}, false, err
	}
	if err := os.Rename(original, quarantine); err != nil {
		return gcMove{}, false, err
	}
	return gcMove{original: original, quarantine: quarantine, directory: directory}, true, nil
}

func rollbackGCMoves(moves []gcMove) error {
	var rollbackErr error
	for index := len(moves) - 1; index >= 0; index-- {
		move := moves[index]
		if _, err := os.Stat(move.quarantine); err != nil {
			if !os.IsNotExist(err) {
				rollbackErr = errors.Join(rollbackErr, err)
			}
			continue
		}
		if _, err := os.Stat(move.original); err == nil {
			rollbackErr = errors.Join(rollbackErr, fmt.Errorf("cannot restore GC quarantine %s: destination exists", move.original))
			continue
		} else if !os.IsNotExist(err) {
			rollbackErr = errors.Join(rollbackErr, err)
			continue
		}
		if err := os.Rename(move.quarantine, move.original); err != nil {
			rollbackErr = errors.Join(rollbackErr, err)
		}
	}
	return rollbackErr
}

func finalizeGCMoves(moves []gcMove) error {
	var removeErr error
	for _, move := range moves {
		var err error
		if move.directory {
			err = os.RemoveAll(move.quarantine)
		} else {
			err = os.Remove(move.quarantine)
			if os.IsNotExist(err) {
				err = nil
			}
		}
		if err != nil {
			removeErr = errors.Join(removeErr, fmt.Errorf("remove GC quarantine %s: %w", move.quarantine, err))
		}
	}
	return removeErr
}

func (s *Service) reconcileGCPath(ctx context.Context, model any, where string, value any, original, quarantine string, directory bool) error {
	var count int64
	if err := s.db.WithContext(ctx).Model(model).Where(where, value).Count(&count).Error; err != nil {
		return err
	}
	if count == 0 {
		return finalizeGCMoves([]gcMove{{original: original, quarantine: quarantine, directory: directory}})
	}
	if _, err := os.Stat(original); err == nil {
		return fmt.Errorf("cannot recover GC quarantine %s: original exists", quarantine)
	} else if !os.IsNotExist(err) {
		return err
	}
	return os.Rename(quarantine, original)
}

// recoverGCQuarantine makes a previous interrupted filesystem/DB transition
// deterministic. Existing metadata restores the source path; absent metadata
// means the committed deletion can be finalized safely.
func (s *Service) recoverGCQuarantine(ctx context.Context) error {
	manifestEntries, err := os.ReadDir(s.manifestsRoot())
	if err != nil && !os.IsNotExist(err) {
		return err
	}
	for _, entry := range manifestEntries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".json"+gcFileSuffix) {
			continue
		}
		releaseID := strings.TrimSuffix(entry.Name(), ".json"+gcFileSuffix)
		quarantine := filepath.Join(s.manifestsRoot(), entry.Name())
		if err := s.reconcileGCPath(ctx, &models.ProfileRelease{}, "id = ?", releaseID, s.manifestPath(releaseID), quarantine, false); err != nil {
			return err
		}
	}

	err = filepath.WalkDir(s.blobsRoot(), func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), gcFileSuffix) {
			return nil
		}
		hash := strings.TrimSuffix(entry.Name(), gcFileSuffix)
		if !validHash(hash) {
			return nil
		}
		return s.reconcileGCPath(ctx, &models.DeliveryBlob{}, "hash_sha256 = ?", hash, s.blobPath(hash), path, false)
	})
	if err != nil && !os.IsNotExist(err) {
		return err
	}

	launcherEntries, err := os.ReadDir(s.launcherRoot)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	for _, entry := range launcherEntries {
		marker := strings.LastIndex(entry.Name(), gcLauncherMarker)
		if !entry.IsDir() || marker < 0 {
			continue
		}
		releaseID := entry.Name()[marker+len(gcLauncherMarker):]
		if releaseID == "" {
			continue
		}
		quarantine := filepath.Join(s.launcherRoot, entry.Name())
		original := filepath.Join(s.launcherRoot, entry.Name()[:marker])
		if err := s.reconcileGCPath(ctx, &models.LauncherRelease{}, "id = ?", releaseID, original, quarantine, true); err != nil {
			return err
		}
	}
	return nil
}

// GarbageCollect retains the newest releases per profile and applies a grace
// period before removing inactive releases, artifacts and unreferenced CAS blobs.
func (s *Service) GarbageCollect(ctx context.Context, keepPerProfile int, grace time.Duration) (GCResult, error) {
	if keepPerProfile < 1 {
		return GCResult{}, errors.New("keep-per-profile must be at least 1")
	}
	if grace < 0 {
		return GCResult{}, errors.New("grace must not be negative")
	}
	s.publishMu.Lock()
	defer s.publishMu.Unlock()

	if err := s.recoverGCQuarantine(ctx); err != nil {
		return GCResult{}, fmt.Errorf("recover interrupted GC: %w", err)
	}
	cutoff := time.Now().UTC().Add(-grace)
	result := GCResult{}
	staged := make([]gcMove, 0)
	err := s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var profileIDs []string
		if err := tx.Model(&models.ProfileRelease{}).Distinct().Pluck("profile_id", &profileIDs).Error; err != nil {
			return err
		}
		for _, profileID := range profileIDs {
			var releases []models.ProfileRelease
			if err := tx.Where("profile_id = ?", profileID).Order("sequence desc").Find(&releases).Error; err != nil {
				return err
			}
			for index, release := range releases {
				if index < keepPerProfile || release.IsActive || !release.CreatedAt.Before(cutoff) {
					continue
				}
				var seedReferences int64
				if err := tx.Model(&models.DeliveryJob{}).
					Where("source_release_id = ? AND status IN ?", release.ID, []string{"queued", "running"}).
					Count(&seedReferences).Error; err != nil {
					return err
				}
				if seedReferences > 0 {
					continue
				}
				manifest := s.manifestPath(release.ID)
				move, moved, err := stageGCPath(manifest, manifest+gcFileSuffix, false)
				if err != nil {
					return err
				}
				if moved {
					staged = append(staged, move)
				}
				var fileIDs []string
				if err := tx.Model(&models.ProfileReleaseFile{}).Where("release_id = ?", release.ID).Pluck("id", &fileIDs).Error; err != nil {
					return err
				}
				if len(fileIDs) > 0 {
					if err := tx.Where("file_id IN ?", fileIDs).Delete(&models.ProfileReleaseFileChunk{}).Error; err != nil {
						return err
					}
				}
				if err := tx.Where("release_id = ?", release.ID).Delete(&models.ProfileReleaseFile{}).Error; err != nil {
					return err
				}
				if err := tx.Delete(&models.ProfileRelease{}, "id = ?", release.ID).Error; err != nil {
					return err
				}
				result.ProfileReleases++
			}
		}

		var artifacts []models.LauncherDeliveryArtifact
		if err := tx.Table("launcher_delivery_artifacts AS artifacts").
			Select("artifacts.*").
			Joins("LEFT JOIN launcher_releases AS releases ON releases.id = artifacts.release_id").
			Where("releases.id IS NULL OR releases.is_active = ?", false).
			Scan(&artifacts).Error; err != nil {
			return err
		}
		launcherCandidates := make(map[string]struct{})
		for _, artifact := range artifacts {
			if !artifact.CreatedAt.Before(cutoff) {
				continue
			}
			if err := tx.Where("artifact_id = ?", artifact.ID).Delete(&models.LauncherDeliveryArtifactChunk{}).Error; err != nil {
				return err
			}
			if err := tx.Delete(&models.LauncherDeliveryArtifact{}, "id = ?", artifact.ID).Error; err != nil {
				return err
			}
			launcherCandidates[artifact.ReleaseID] = struct{}{}
			result.LauncherArtifacts++
		}
		for releaseID := range launcherCandidates {
			var remaining int64
			if err := tx.Model(&models.LauncherDeliveryArtifact{}).Where("release_id = ?", releaseID).Count(&remaining).Error; err != nil {
				return err
			}
			if remaining != 0 {
				continue
			}
			var release models.LauncherRelease
			if err := tx.Where("id = ? AND is_active = ? AND ((published_at IS NOT NULL AND published_at < ?) OR (published_at IS NULL AND created_at < ?))", releaseID, false, cutoff, cutoff).First(&release).Error; err != nil {
				if errors.Is(err, gorm.ErrRecordNotFound) {
					continue
				}
				return err
			}
			source := filepath.Join(s.launcherRoot, release.Version)
			move, moved, err := stageGCPath(source, source+gcLauncherMarker+release.ID, true)
			if err != nil {
				return err
			}
			if moved {
				staged = append(staged, move)
			}
			if err := tx.Where("release_id = ?", release.ID).Delete(&models.LauncherReleaseFile{}).Error; err != nil {
				return err
			}
			if err := tx.Where("kind = ? AND generation = ?", "launcher", release.ID).Delete(&models.DeliveryJob{}).Error; err != nil {
				return err
			}
			if err := tx.Delete(&models.LauncherRelease{}, "id = ?", release.ID).Error; err != nil {
				return err
			}
			result.LauncherReleases++
		}
		return nil
	})
	if err != nil {
		return GCResult{}, errors.Join(err, rollbackGCMoves(staged))
	}
	if err := finalizeGCMoves(staged); err != nil {
		return result, err
	}

	referenced := make(map[string]struct{})
	var hashes []string
	if err := s.db.WithContext(ctx).Model(&models.ProfileReleaseFileChunk{}).Distinct().Pluck("hash_sha256", &hashes).Error; err != nil {
		return result, err
	}
	for _, hash := range hashes {
		referenced[hash] = struct{}{}
	}
	hashes = nil
	if err := s.db.WithContext(ctx).Model(&models.LauncherDeliveryArtifactChunk{}).Distinct().Pluck("hash_sha256", &hashes).Error; err != nil {
		return result, err
	}
	for _, hash := range hashes {
		referenced[hash] = struct{}{}
	}
	var blobs []models.DeliveryBlob
	if err := s.db.WithContext(ctx).Find(&blobs).Error; err != nil {
		return result, err
	}
	for _, blob := range blobs {
		if !blob.CreatedAt.Before(cutoff) {
			continue
		}
		if _, exists := referenced[blob.HashSHA256]; exists {
			continue
		}
		var stagedBlob []gcMove
		err := s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
			path := s.blobPath(blob.HashSHA256)
			move, moved, err := stageGCPath(path, path+gcFileSuffix, false)
			if err != nil {
				return err
			}
			if moved {
				stagedBlob = append(stagedBlob, move)
			}
			return tx.Delete(&models.DeliveryBlob{}, "hash_sha256 = ?", blob.HashSHA256).Error
		})
		if err != nil {
			return result, errors.Join(err, rollbackGCMoves(stagedBlob))
		}
		result.Blobs++
		if err := finalizeGCMoves(stagedBlob); err != nil {
			return result, err
		}
	}
	return result, nil
}
