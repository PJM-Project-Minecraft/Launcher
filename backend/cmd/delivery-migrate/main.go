package main

import (
	"context"
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
	cfg := config.Load()
	if err := cfg.Validate(); err != nil {
		log.Fatal(err)
	}
	db, err := database.Open(cfg)
	if err != nil {
		log.Fatal(err)
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
	fmt.Printf("DELIVERY_MANIFEST_PUBKEY=%s\n", service.SigningPublicKeyHex())
}
