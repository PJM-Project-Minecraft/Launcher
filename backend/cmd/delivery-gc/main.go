package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"time"

	"launcher-backend/internal/config"
	"launcher-backend/internal/database"
	"launcher-backend/internal/delivery"
)

func main() {
	keep := flag.Int("keep-profile-releases", 3, "minimum number of newest releases retained per profile")
	grace := flag.Duration("grace", 7*24*time.Hour, "minimum age before unreachable data can be removed")
	flag.Parse()

	cfg := config.Load()
	db, err := database.Open(cfg)
	if err != nil {
		log.Fatal(err)
	}
	service, err := delivery.NewService(db, cfg.DeliveryRoot, cfg.ProfileStorageRoot, cfg.LauncherReleaseRoot, cfg.DeliveryManifestSigningKey)
	if err != nil {
		log.Fatal(err)
	}
	result, err := service.GarbageCollect(context.Background(), *keep, *grace)
	if err != nil {
		log.Fatal(err)
	}
	fmt.Printf("removed profile_releases=%d launcher_artifacts=%d blobs=%d\n", result.ProfileReleases, result.LauncherArtifacts, result.Blobs)
}
