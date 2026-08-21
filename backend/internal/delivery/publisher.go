package delivery

import (
	"context"
	"crypto/ed25519"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"launcher-backend/internal/events"
	"launcher-backend/internal/models"

	"gorm.io/gorm"
)

const deliveryEvent = "delivery-v2"

var defaultPreservePaths = []string{
	"saves/", "resourcepacks/", "shaderpacks/", "screenshots/", "logs/",
	"crash-reports/", "options.txt", "optionsof.txt", "servers.dat",
}

type Watcher struct {
	service *Service
	broker  *events.Broker
}

func NewWatcher(service *Service, broker *events.Broker) *Watcher {
	return &Watcher{service: service, broker: broker}
}

func (w *Watcher) Start() {
	_ = w.service.db.Model(&models.DeliveryJob{}).
		Where("status IN ?", []string{"queued", "running"}).
		Updates(map[string]any{"status": "queued", "phase": "recovery", "message": "Возобновляем после перезапуска"}).Error
	go func() {
		ticker := time.NewTicker(2 * time.Second)
		defer ticker.Stop()
		for {
			w.reconcile()
			select {
			case <-w.service.stop:
				return
			case <-ticker.C:
			}
		}
	}()
}

func (w *Watcher) reconcile() {
	profiles, err := os.ReadDir(w.service.incomingRoot())
	if err != nil {
		return
	}
	for _, profileEntry := range profiles {
		if !profileEntry.IsDir() {
			continue
		}
		profileID := profileEntry.Name()
		root := filepath.Join(w.service.incomingRoot(), profileID)
		generations, readErr := os.ReadDir(root)
		if readErr != nil {
			continue
		}
		sort.Slice(generations, func(i, j int) bool { return generations[i].Name() < generations[j].Name() })
		for _, generation := range generations {
			if !generation.IsDir() || (!strings.HasSuffix(generation.Name(), ".ready") && !strings.HasSuffix(generation.Name(), ".processing")) {
				continue
			}
			name := strings.TrimSuffix(strings.TrimSuffix(generation.Name(), ".ready"), ".processing")
			w.claimAndPublish(profileID, name, filepath.Join(root, generation.Name()))
		}
	}
}

func (w *Watcher) claimAndPublish(profileID, generation, source string) {
	w.service.publishMu.Lock()
	defer w.service.publishMu.Unlock()

	processing := filepath.Join(filepath.Dir(source), generation+".processing")
	if strings.HasSuffix(source, ".ready") {
		if err := os.Rename(source, processing); err != nil {
			return
		}
	}
	var existing models.DeliveryJob
	err := w.service.db.Where("kind = ? AND profile_id = ? AND generation = ?", "profile", profileID, generation).First(&existing).Error
	if err == nil && (existing.Status == "succeeded" || existing.Status == "failed") {
		return
	}
	job := existing
	if errors.Is(err, gorm.ErrRecordNotFound) {
		job = models.DeliveryJob{ID: newID(), Kind: "profile", ProfileID: profileID, Generation: generation, Status: "queued", Phase: "claimed", Message: "SFTP generation принята"}
		if w.service.db.Create(&job).Error != nil {
			return
		}
	}
	started := time.Now().UTC()
	w.updateJob(&job, map[string]any{"status": "running", "phase": "scan", "message": "Проверяем и чанкуем файлы", "started_at": &started, "error": ""})
	releaseID, publishErr := w.service.publishProfile(context.Background(), profileID, processing, func(current, total int) {
		progress := 0.05
		if total > 0 {
			progress = 0.05 + 0.80*float64(current)/float64(total)
		}
		w.updateJob(&job, map[string]any{"phase": "chunks", "message": fmt.Sprintf("Обработано файлов: %d / %d", current, total), "progress": progress})
	})
	ended := time.Now().UTC()
	if publishErr != nil {
		w.updateJob(&job, map[string]any{"status": "failed", "phase": "failed", "message": "Публикация отклонена", "error": publishErr.Error(), "ended_at": &ended})
		failed := filepath.Join(filepath.Dir(processing), generation+".failed")
		_ = os.Rename(processing, failed)
		slog.Error("delivery v2 publication failed", "profile", profileID, "generation", generation, "error", publishErr)
		return
	}
	w.updateJob(&job, map[string]any{"status": "succeeded", "phase": "done", "message": "Immutable release активирован", "progress": 1.0, "release_id": releaseID, "ended_at": &ended})
	consumed := filepath.Join(filepath.Dir(processing), generation+".published")
	_ = os.Rename(processing, consumed)
	if w.broker != nil {
		w.broker.Publish(deliveryEvent)
	}
}

func (w *Watcher) updateJob(job *models.DeliveryJob, changes map[string]any) {
	changes["updated_at"] = time.Now().UTC()
	_ = w.service.db.Model(&models.DeliveryJob{}).Where("id = ?", job.ID).Updates(changes).Error
	if w.broker != nil {
		w.broker.Publish(deliveryEvent)
	}
}

func (s *Service) CreateDraft(ctx context.Context, profileID string) (string, string, error) {
	var profile models.Profile
	if err := s.db.WithContext(ctx).Select("id").First(&profile, "id = ?", profileID).Error; err != nil {
		return "", "", err
	}
	generation := time.Now().UTC().Format("20060102T150405Z") + "-" + newID()
	path := filepath.Join(s.incomingRoot(), profileID, generation+".upload")
	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		return "", "", err
	}
	if err := os.Mkdir(path, 0755); err != nil {
		return "", "", err
	}
	return generation, path, nil
}

func (s *Service) publishProfile(ctx context.Context, profileID, source string, progress func(int, int)) (string, error) {
	var profile models.Profile
	if err := s.db.WithContext(ctx).First(&profile, "id = ?", profileID).Error; err != nil {
		return "", err
	}
	files, total, err := s.collectFiles(ctx, source, progress)
	if err != nil {
		return "", err
	}
	if len(files) == 0 {
		return "", errors.New("ready generation is empty")
	}
	var maxSequence int
	_ = s.db.WithContext(ctx).Model(&models.ProfileRelease{}).Where("profile_id = ?", profileID).Select("COALESCE(MAX(sequence), 0)").Scan(&maxSequence).Error
	releaseID := newID()
	createdAt := time.Now().UTC()
	config := s.profileConfig(profile)
	if len(config.PreservePaths) == 0 {
		config.PreservePaths = append([]string(nil), defaultPreservePaths...)
	}
	manifest := ProfileManifest{SchemaVersion: SchemaVersion, Kind: "profile", ReleaseID: releaseID, Sequence: maxSequence + 1, CreatedAt: createdAt, Profile: config, Files: files, FileCount: len(files), TotalSize: total}
	manifestBytes, manifestHash, err := encodeManifest(manifest)
	if err != nil {
		return "", err
	}
	signature := ""
	if len(s.signingKey) != 0 {
		signature = hex.EncodeToString(ed25519.Sign(s.signingKey, manifestBytes))
	}
	if err := writeAtomic(s.manifestPath(releaseID), manifestBytes); err != nil {
		return "", err
	}
	release := models.ProfileRelease{ID: releaseID, ProfileID: profileID, Sequence: manifest.Sequence, ManifestSHA256: manifestHash, ManifestSignature: signature, FileCount: len(files), TotalSize: total, IsActive: true, CreatedAt: createdAt}
	err = s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		if err := tx.Model(&models.ProfileRelease{}).Where("profile_id = ? AND is_active = ?", profileID, true).Update("is_active", false).Error; err != nil {
			return err
		}
		if err := tx.Create(&release).Error; err != nil {
			return err
		}
		for _, file := range files {
			row := models.ProfileReleaseFile{ID: newID(), ReleaseID: releaseID, Path: file.Path, HashSHA256: file.SHA256, Size: file.Size, Executable: file.Executable}
			if err := tx.Create(&row).Error; err != nil {
				return err
			}
			chunks := make([]models.ProfileReleaseFileChunk, 0, len(file.Chunks))
			for ordinal, chunk := range file.Chunks {
				chunks = append(chunks, models.ProfileReleaseFileChunk{ID: newID(), FileID: row.ID, Ordinal: ordinal, HashSHA256: chunk.SHA256, Size: chunk.Size})
			}
			if len(chunks) > 0 {
				if err := tx.CreateInBatches(&chunks, 100).Error; err != nil {
					return err
				}
			}
		}
		return tx.Model(&models.Profile{}).Where("id = ?", profileID).Updates(map[string]any{"manifest_version": manifest.Sequence, "manifest_updated_at": createdAt}).Error
	})
	if err != nil {
		_ = os.Remove(s.manifestPath(releaseID))
		return "", err
	}
	return releaseID, nil
}

// ImportLauncherRelease chunks an already validated legacy release artifact.
// The old release row remains the version/mandatory source during the bridge.
func (s *Service) ImportLauncherRelease(ctx context.Context, release models.LauncherRelease) error {
	s.publishMu.Lock()
	defer s.publishMu.Unlock()
	for _, file := range release.Files {
		path := filepath.Join(s.launcherRoot, release.Version, file.Platform, file.FileName)
		artifact, err := s.chunkFile(ctx, path)
		if err != nil {
			return err
		}
		if artifact.SHA256 != strings.ToLower(file.HashSHA256) || artifact.Size != file.Size {
			return errors.New("launcher artifact differs from uploaded release metadata")
		}
		row := models.LauncherDeliveryArtifact{ID: newID(), ReleaseID: release.ID, Platform: file.Platform, HashSHA256: artifact.SHA256, Size: artifact.Size, Executable: true, CreatedAt: time.Now().UTC()}
		err = s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
			var existing []models.LauncherDeliveryArtifact
			if err := tx.Select("id").Where("release_id = ? AND platform = ?", release.ID, file.Platform).Find(&existing).Error; err != nil {
				return err
			}
			for _, item := range existing {
				if err := tx.Where("artifact_id = ?", item.ID).Delete(&models.LauncherDeliveryArtifactChunk{}).Error; err != nil {
					return err
				}
			}
			if err := tx.Where("release_id = ? AND platform = ?", release.ID, file.Platform).Delete(&models.LauncherDeliveryArtifact{}).Error; err != nil {
				return err
			}
			if err := tx.Create(&row).Error; err != nil {
				return err
			}
			chunks := make([]models.LauncherDeliveryArtifactChunk, 0, len(artifact.Chunks))
			for ordinal, chunk := range artifact.Chunks {
				chunks = append(chunks, models.LauncherDeliveryArtifactChunk{ID: newID(), ArtifactID: row.ID, Ordinal: ordinal, HashSHA256: chunk.SHA256, Size: chunk.Size})
			}
			return tx.CreateInBatches(&chunks, 100).Error
		})
		if err != nil {
			return err
		}
	}
	return nil
}
