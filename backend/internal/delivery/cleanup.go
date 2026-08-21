package delivery

import (
	"context"
	"errors"
	"os"
	"time"

	"launcher-backend/internal/models"

	"gorm.io/gorm"
)

// GCResult describes only records that were made unreachable by an explicit
// operator-run cleanup. Garbage collection is never started by the server.
type GCResult struct {
	ProfileReleases   int `json:"profileReleases"`
	LauncherArtifacts int `json:"launcherArtifacts"`
	Blobs             int `json:"blobs"`
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

	cutoff := time.Now().UTC().Add(-grace)
	result := GCResult{}
	manifestPaths := make([]string, 0)
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
				manifestPaths = append(manifestPaths, s.manifestPath(release.ID))
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
			result.LauncherArtifacts++
		}
		return nil
	})
	if err != nil {
		return GCResult{}, err
	}
	for _, path := range manifestPaths {
		_ = os.Remove(path)
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
		if err := s.db.WithContext(ctx).Delete(&models.DeliveryBlob{}, "hash_sha256 = ?", blob.HashSHA256).Error; err != nil {
			return result, err
		}
		_ = os.Remove(s.blobPath(blob.HashSHA256))
		result.Blobs++
	}
	return result, nil
}
