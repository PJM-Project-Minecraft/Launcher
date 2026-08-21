package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"path/filepath"

	"launcher-backend/internal/config"
	"launcher-backend/internal/database"
	"launcher-backend/internal/delivery"
	"launcher-backend/internal/models"
)

// This expensive backfill is never run by server startup or deploy scripts.
func main() {
	dryRun := flag.Bool("dry-run", false, "validate legacy sources and print the migration size without writing")
	verifyOnly := flag.Bool("verify-only", false, "verify signed v2 manifests and reconstruct active files from CAS without writing")
	flag.Parse()
	if *dryRun && *verifyOnly {
		log.Fatal("--dry-run and --verify-only are mutually exclusive")
	}
	cfg := config.Load()
	if err := cfg.Validate(); err != nil {
		log.Fatal(err)
	}
	db, err := database.Open(cfg)
	if err != nil {
		log.Fatal(err)
	}
	if *verifyOnly {
		service, err := delivery.NewService(db, cfg.DeliveryRoot, cfg.ProfileStorageRoot, cfg.LauncherReleaseRoot, cfg.DeliveryManifestSigningKey)
		if err != nil {
			log.Fatal(err)
		}
		audit, err := service.AuditMigration(context.Background())
		if err != nil {
			log.Fatal(err)
		}
		fmt.Printf("verified profile_releases=%d profile_files=%d launcher_releases=%d launcher_files=%d bytes=%d\n", audit.ProfileReleases, audit.ProfileFiles, audit.LauncherReleases, audit.LauncherFiles, audit.VerifiedBytes)
		return
	}
	plan, err := delivery.InspectMigrationSources(context.Background(), db, cfg.ProfileStorageRoot, cfg.LauncherReleaseRoot)
	if err != nil {
		log.Fatal(err)
	}
	fmt.Printf("preflight profiles=%d profile_files=%d profile_bytes=%d launcher_releases=%d launcher_files=%d launcher_bytes=%d unsigned_legacy_launcher_files=%d required_bytes=%d\n",
		plan.Profiles, plan.ProfileFiles, plan.ProfileBytes, plan.LauncherReleases, plan.LauncherFiles, plan.LauncherBytes, plan.UnsignedLegacyLauncherFiles, plan.RequiredBytes)
	if *dryRun {
		fmt.Println("dry-run: no schema, manifest, CAS or job changes were made")
		return
	}
	if err := database.AutoMigrate(db); err != nil {
		log.Fatal(err)
	}
	service, err := delivery.NewService(db, cfg.DeliveryRoot, cfg.ProfileStorageRoot, cfg.LauncherReleaseRoot, cfg.DeliveryManifestSigningKey)
	if err != nil {
		log.Fatal(err)
	}

	var profiles []models.Profile
	if err := db.Where("is_active = ?", true).Find(&profiles).Error; err != nil {
		log.Fatal(err)
	}
	for _, profile := range profiles {
		releaseID, err := service.BackfillProfile(context.Background(), profile.ID, filepath.Join(cfg.ProfileStorageRoot, profile.Slug, "files"))
		if err != nil {
			log.Fatalf("profile %s: %v", profile.ID, err)
		}
		fmt.Printf("profile %s -> %s\n", profile.ID, releaseID)
	}

	var releases []models.LauncherRelease
	if err := db.Preload("Files").Where("is_active = ?", true).Find(&releases).Error; err != nil {
		log.Fatal(err)
	}
	for _, release := range releases {
		if err := service.ImportLauncherRelease(context.Background(), release); err != nil {
			log.Fatalf("launcher %s: %v", release.Version, err)
		}
		fmt.Printf("launcher %s -> v2\n", release.Version)
	}
	audit, err := service.AuditMigration(context.Background())
	if err != nil {
		log.Fatal(err)
	}
	fmt.Printf("verified profile_releases=%d profile_files=%d launcher_releases=%d launcher_files=%d bytes=%d\n", audit.ProfileReleases, audit.ProfileFiles, audit.LauncherReleases, audit.LauncherFiles, audit.VerifiedBytes)
	fmt.Printf("DELIVERY_MANIFEST_PUBKEY=%s\n", service.SigningPublicKeyHex())
}
