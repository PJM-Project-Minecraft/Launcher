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
	"strings"

	"launcher-backend/internal/models"

	"gorm.io/gorm"
)

// MigrationPlan is a read-only inventory of legacy sources that delivery v2
// will import. RequiredBytes is a conservative CAS upper bound before dedup.
type MigrationPlan struct {
	Profiles                    int   `json:"profiles"`
	ProfileFiles                int   `json:"profileFiles"`
	ProfileBytes                int64 `json:"profileBytes"`
	LauncherReleases            int   `json:"launcherReleases"`
	LauncherFiles               int   `json:"launcherFiles"`
	LauncherBytes               int64 `json:"launcherBytes"`
	UnsignedLegacyLauncherFiles int   `json:"unsignedLegacyLauncherFiles"`
	RequiredBytes               int64 `json:"requiredBytes"`
}

type MigrationAudit struct {
	ProfileReleases  int   `json:"profileReleases"`
	ProfileFiles     int   `json:"profileFiles"`
	LauncherReleases int   `json:"launcherReleases"`
	LauncherFiles    int   `json:"launcherFiles"`
	VerifiedBytes    int64 `json:"verifiedBytes"`
}

func inspectPortableTree(root string) (int, int64, error) {
	info, err := os.Stat(root)
	if err != nil {
		return 0, 0, err
	}
	if !info.IsDir() {
		return 0, 0, errors.New("profile source is not a directory")
	}
	seen := make(map[string]string)
	files := 0
	var bytes int64
	err = filepath.WalkDir(root, func(path string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if path == root || entry.IsDir() {
			return nil
		}
		if entry.Type()&os.ModeSymlink != 0 {
			return fmt.Errorf("symlink is forbidden: %s", path)
		}
		metadata, err := entry.Info()
		if err != nil {
			return err
		}
		if !metadata.Mode().IsRegular() {
			return fmt.Errorf("non-regular file is forbidden: %s", path)
		}
		relative, err := safeRelative(root, path)
		if err != nil {
			return err
		}
		folded := strings.ToLower(relative)
		if previous, exists := seen[folded]; exists {
			return fmt.Errorf("case-conflicting paths: %s and %s", previous, relative)
		}
		seen[folded] = relative
		files++
		bytes += metadata.Size()
		return nil
	})
	if err != nil {
		return 0, 0, err
	}
	if files == 0 {
		return 0, 0, errors.New("profile source is empty")
	}
	return files, bytes, nil
}

func verifyLegacyLauncherFile(path string, file models.LauncherReleaseFile) error {
	if !validHash(strings.ToLower(file.HashSHA256)) || file.HashSHA256 != strings.ToLower(file.HashSHA256) {
		return errors.New("invalid SHA-256 metadata")
	}
	metadata, err := os.Lstat(path)
	if err != nil {
		return err
	}
	if !metadata.Mode().IsRegular() || metadata.Mode()&os.ModeSymlink != 0 {
		return errors.New("launcher source is not a regular file")
	}
	if metadata.Size() != file.Size {
		return fmt.Errorf("size mismatch: disk=%d database=%d", metadata.Size(), file.Size)
	}
	input, err := os.Open(path)
	if err != nil {
		return err
	}
	defer input.Close()
	hash := sha256.New()
	if _, err := io.Copy(hash, input); err != nil {
		return err
	}
	if hex.EncodeToString(hash.Sum(nil)) != file.HashSHA256 {
		return errors.New("SHA-256 mismatch")
	}
	if file.SignatureEd25519 != "" {
		signature, err := hex.DecodeString(file.SignatureEd25519)
		if err != nil || len(signature) != 64 || strings.ToLower(file.SignatureEd25519) != file.SignatureEd25519 {
			return errors.New("invalid Ed25519 signature metadata")
		}
	}
	return nil
}

// InspectMigrationSources validates every byte source without creating v2
// tables, manifests, jobs or CAS blobs. It is safe to run before deployment.
func InspectMigrationSources(ctx context.Context, db *gorm.DB, profileRoot, launcherRoot string) (MigrationPlan, error) {
	plan := MigrationPlan{}
	var profiles []models.Profile
	if err := db.WithContext(ctx).Where("is_active = ?", true).Order("created_at asc").Find(&profiles).Error; err != nil {
		return plan, err
	}
	for _, profile := range profiles {
		if strings.TrimSpace(profile.LaunchCommandWindows) == "" || strings.TrimSpace(profile.LaunchCommandLinux) == "" {
			return plan, fmt.Errorf("profile %s must define Windows and Linux launch commands", profile.ID)
		}
		root := filepath.Join(profileRoot, profile.Slug, "files")
		files, bytes, err := inspectPortableTree(root)
		if err != nil {
			return plan, fmt.Errorf("profile %s (%s): %w", profile.ID, profile.Slug, err)
		}
		plan.Profiles++
		plan.ProfileFiles += files
		plan.ProfileBytes += bytes
	}

	var releases []models.LauncherRelease
	if err := db.WithContext(ctx).Preload("Files").Where("is_active = ?", true).Find(&releases).Error; err != nil {
		return plan, err
	}
	sort.Slice(releases, func(left, right int) bool {
		return compareVersion(releases[left].Version, releases[right].Version) > 0
	})
	for releaseIndex, release := range releases {
		if _, valid := parseVersion(release.Version); !valid {
			return plan, fmt.Errorf("launcher release %s has invalid semantic version", release.Version)
		}
		platforms := make(map[string]bool)
		for _, file := range release.Files {
			if file.Platform != "linux-x64" && file.Platform != "windows-x64" {
				return plan, fmt.Errorf("launcher %s has unsupported platform %s", release.Version, file.Platform)
			}
			if platforms[file.Platform] {
				return plan, fmt.Errorf("launcher %s has duplicate platform %s", release.Version, file.Platform)
			}
			platforms[file.Platform] = true
			path := filepath.Join(launcherRoot, release.Version, file.Platform, file.FileName)
			if err := verifyLegacyLauncherFile(path, file); err != nil {
				return plan, fmt.Errorf("launcher %s %s: %w", release.Version, file.Platform, err)
			}
			if file.SignatureEd25519 == "" {
				plan.UnsignedLegacyLauncherFiles++
			}
			plan.LauncherFiles++
			plan.LauncherBytes += file.Size
		}
		if releaseIndex == 0 {
			for _, platform := range []string{"linux-x64", "windows-x64"} {
				if !platforms[platform] {
					return plan, fmt.Errorf("current launcher %s is missing %s", release.Version, platform)
				}
				for _, file := range release.Files {
					if file.Platform == platform && file.SignatureEd25519 == "" {
						return plan, fmt.Errorf("current launcher %s %s is unsigned", release.Version, platform)
					}
				}
			}
		}
		plan.LauncherReleases++
	}
	if plan.Profiles == 0 {
		return plan, errors.New("no active profiles to migrate")
	}
	if plan.LauncherReleases == 0 {
		return plan, errors.New("no active launcher releases to migrate")
	}
	plan.RequiredBytes = plan.ProfileBytes + plan.LauncherBytes
	return plan, nil
}

func (s *Service) auditReleaseFile(ctx context.Context, file ReleaseFile) error {
	whole := sha256.New()
	var total int64
	for _, chunk := range file.Chunks {
		if !validHash(chunk.SHA256) || chunk.Size <= 0 {
			return errors.New("invalid chunk reference")
		}
		var blob models.DeliveryBlob
		if err := s.db.WithContext(ctx).First(&blob, "hash_sha256 = ?", chunk.SHA256).Error; err != nil {
			return err
		}
		if blob.Size != chunk.Size {
			return errors.New("chunk size differs from CAS metadata")
		}
		input, err := os.Open(s.blobPath(chunk.SHA256))
		if err != nil {
			return err
		}
		chunkHash := sha256.New()
		written, copyErr := io.Copy(io.MultiWriter(whole, chunkHash), input)
		closeErr := input.Close()
		if copyErr != nil {
			return copyErr
		}
		if closeErr != nil {
			return closeErr
		}
		if written != chunk.Size || hex.EncodeToString(chunkHash.Sum(nil)) != chunk.SHA256 {
			return errors.New("CAS chunk content verification failed")
		}
		total += written
	}
	if total != file.Size || hex.EncodeToString(whole.Sum(nil)) != file.SHA256 {
		return errors.New("reconstructed file differs from descriptor")
	}
	return nil
}

func (s *Service) verifySignedDocument(body []byte, expectedHash, signature string) error {
	digest := sha256.Sum256(body)
	if hex.EncodeToString(digest[:]) != expectedHash {
		return errors.New("signed document SHA-256 mismatch")
	}
	if len(s.signingKey) == 0 {
		return errors.New("delivery signing key is unavailable")
	}
	signatureBytes, err := hex.DecodeString(signature)
	if err != nil || len(signatureBytes) != 64 {
		return errors.New("signed document has invalid Ed25519 signature")
	}
	if !ed25519.Verify(s.signingKey.Public().(ed25519.PublicKey), body, signatureBytes) {
		return errors.New("signed document Ed25519 verification failed")
	}
	return nil
}

// AuditMigration reconstructs every active v2 file from CAS and verifies the
// signed immutable documents. It is read-only and intended as the post-import
// gate before a v2 launcher is published.
func (s *Service) AuditMigration(ctx context.Context) (MigrationAudit, error) {
	audit := MigrationAudit{}
	var profiles []models.Profile
	if err := s.db.WithContext(ctx).Where("is_active = ?", true).Find(&profiles).Error; err != nil {
		return audit, err
	}
	for _, profile := range profiles {
		var release models.ProfileRelease
		if err := s.db.WithContext(ctx).Where("profile_id = ? AND is_active = ?", profile.ID, true).First(&release).Error; err != nil {
			return audit, fmt.Errorf("profile %s has no active v2 release: %w", profile.ID, err)
		}
		body, err := os.ReadFile(s.manifestPath(release.ID))
		if err != nil {
			return audit, err
		}
		if err := s.verifySignedDocument(body, release.ManifestSHA256, release.ManifestSignature); err != nil {
			return audit, fmt.Errorf("profile release %s: %w", release.ID, err)
		}
		var manifest ProfileManifest
		if err := json.Unmarshal(body, &manifest); err != nil {
			return audit, err
		}
		if manifest.SchemaVersion != SchemaVersion || manifest.ReleaseID != release.ID || manifest.Profile.ID != profile.ID || manifest.FileCount != len(manifest.Files) {
			return audit, fmt.Errorf("profile release %s manifest metadata mismatch", release.ID)
		}
		var manifestBytes int64
		for _, file := range manifest.Files {
			if err := s.auditReleaseFile(ctx, file); err != nil {
				return audit, fmt.Errorf("profile release %s file %s: %w", release.ID, file.Path, err)
			}
			audit.ProfileFiles++
			audit.VerifiedBytes += file.Size
			manifestBytes += file.Size
		}
		if release.FileCount != manifest.FileCount || release.TotalSize != manifest.TotalSize || manifest.TotalSize != manifestBytes {
			return audit, fmt.Errorf("profile release %s aggregate metadata mismatch", release.ID)
		}
		audit.ProfileReleases++
	}

	var releases []models.LauncherRelease
	if err := s.db.WithContext(ctx).Preload("Files").Where("is_active = ?", true).Find(&releases).Error; err != nil {
		return audit, err
	}
	for _, release := range releases {
		for _, sourceFile := range release.Files {
			var artifact models.LauncherDeliveryArtifact
			if err := s.db.WithContext(ctx).Where("release_id = ? AND platform = ?", release.ID, sourceFile.Platform).First(&artifact).Error; err != nil {
				return audit, fmt.Errorf("launcher %s %s has no v2 artifact: %w", release.Version, sourceFile.Platform, err)
			}
			body := []byte(artifact.DescriptorJSON)
			if err := s.verifySignedDocument(body, artifact.DescriptorSHA256, artifact.DescriptorSignature); err != nil {
				return audit, fmt.Errorf("launcher %s %s: %w", release.Version, sourceFile.Platform, err)
			}
			var descriptor LauncherManifest
			if err := json.Unmarshal(body, &descriptor); err != nil {
				return audit, err
			}
			if descriptor.SchemaVersion != SchemaVersion || descriptor.ReleaseID != release.ID || descriptor.Version != release.Version || descriptor.Platform != sourceFile.Platform || descriptor.ArtifactSignature != sourceFile.SignatureEd25519 {
				return audit, fmt.Errorf("launcher %s %s descriptor metadata mismatch", release.Version, sourceFile.Platform)
			}
			if artifact.HashSHA256 != descriptor.Artifact.SHA256 || artifact.Size != descriptor.Artifact.Size || sourceFile.HashSHA256 != descriptor.Artifact.SHA256 || sourceFile.Size != descriptor.Artifact.Size {
				return audit, fmt.Errorf("launcher %s %s artifact metadata mismatch", release.Version, sourceFile.Platform)
			}
			if err := s.auditReleaseFile(ctx, descriptor.Artifact); err != nil {
				return audit, fmt.Errorf("launcher %s %s: %w", release.Version, sourceFile.Platform, err)
			}
			audit.LauncherFiles++
			audit.VerifiedBytes += descriptor.Artifact.Size
		}
		audit.LauncherReleases++
	}
	return audit, nil
}
