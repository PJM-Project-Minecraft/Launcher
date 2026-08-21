package config

import (
	"strings"
	"testing"
	"time"
)

const testDeliverySigningKey = "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"

func TestValidateRejectsDevSecretsInProduction(t *testing.T) {
	cfg := Config{AppEnv: "production", JWTSecret: "dev-only-change-me"}
	if err := cfg.Validate(); err == nil {
		t.Fatal("production с dev JWT-секретом должен отклоняться")
	}
	cfg.JWTSecret = "change-me-in-production"
	if err := cfg.Validate(); err == nil {
		t.Fatal("production с compose-заглушкой JWT-секрета должен отклоняться")
	}
}

func TestValidateAllowsDevSecretsInDevelopment(t *testing.T) {
	cfg := Config{AppEnv: "development", JWTSecret: "dev-only-change-me"}
	if err := cfg.Validate(); err != nil {
		t.Fatalf("development должен работать с дефолтным секретом: %v", err)
	}
}

func TestValidateAllowsRealSecretInProduction(t *testing.T) {
	cfg := Config{
		AppEnv:                     "production",
		JWTSecret:                  "a-real-32-char-random-secret-value",
		AnticheatSecret:            "a-distinct-anticheat-secret-value",
		GameAPISecret:              "a-distinct-game-api-secret-value",
		SiteOrderSecret:            "a-distinct-site-order-secret-value",
		DatabaseURL:                "postgres://user:pass@127.0.0.1:5432/launcher?sslmode=disable",
		DeliveryManifestSigningKey: testDeliverySigningKey,
	}
	if err := cfg.Validate(); err != nil {
		t.Fatalf("production с нормальными секретами должен проходить: %v", err)
	}
}

func TestValidateRejectsEmptyDatabaseURLInProduction(t *testing.T) {
	cfg := Config{
		AppEnv:                     "production",
		JWTSecret:                  "a-real-32-char-random-secret-value",
		AnticheatSecret:            "a-distinct-anticheat-secret-value",
		GameAPISecret:              "a-distinct-game-api-secret-value",
		SiteOrderSecret:            "a-distinct-site-order-secret-value",
		DeliveryManifestSigningKey: testDeliverySigningKey,
		// DatabaseURL пуст → тихий SQLite-fallback, запрещён в проде.
	}
	if err := cfg.Validate(); err == nil {
		t.Fatal("production без DATABASE_URL должен отклоняться (SQLite-fallback запрещён)")
	}
}

func TestValidateAllowsEmptyDatabaseURLInDevelopment(t *testing.T) {
	cfg := Config{AppEnv: "development", JWTSecret: "dev-only-change-me"}
	if err := cfg.Validate(); err != nil {
		t.Fatalf("development с пустым DATABASE_URL (SQLite) должен работать: %v", err)
	}
}

func TestGameAPISecretDerivedFromJWT(t *testing.T) {
	t.Setenv("JWT_SECRET", "some-long-jwt-secret-value")
	t.Setenv("GAME_API_SECRET", "")
	cfg := Load()
	if cfg.GameAPISecret != "game:some-long-jwt-secret-value" {
		t.Fatalf("пустой GAME_API_SECRET должен деривироваться из JWT, получено %q", cfg.GameAPISecret)
	}
}

func TestGameAPISecretFromEnv(t *testing.T) {
	t.Setenv("JWT_SECRET", "some-long-jwt-secret-value")
	t.Setenv("GAME_API_SECRET", "explicit-game-secret")
	cfg := Load()
	if cfg.GameAPISecret != "explicit-game-secret" {
		t.Fatalf("GAME_API_SECRET из env должен использоваться как есть, получено %q", cfg.GameAPISecret)
	}
}

func TestValidateRejectsWeakAnticheatSecret(t *testing.T) {
	jwt := "a-real-32-char-random-secret-value"
	base := Config{AppEnv: "production", JWTSecret: jwt, DeliveryManifestSigningKey: testDeliverySigningKey}

	cases := map[string]string{
		"деривированный из JWT": "anticheat:" + jwt,
		"равен JWT":             jwt,
		"дев-заглушка":          "dev-only-change-me",
	}
	for name, secret := range cases {
		cfg := base
		cfg.AnticheatSecret = secret
		if err := cfg.Validate(); err == nil {
			t.Fatalf("ANTICHEAT_SECRET (%s) должен отклоняться в проде", name)
		}
	}
}

func TestValidateRejectsWeakGameAPISecret(t *testing.T) {
	jwt := "a-real-32-char-random-secret-value"
	base := Config{
		AppEnv:                     "production",
		JWTSecret:                  jwt,
		AnticheatSecret:            "a-distinct-anticheat-secret-value",
		SiteOrderSecret:            "a-distinct-site-order-secret-value",
		DatabaseURL:                "postgres://user:pass@127.0.0.1:5432/launcher?sslmode=disable",
		DeliveryManifestSigningKey: testDeliverySigningKey,
	}

	cases := map[string]string{
		"деривированный из JWT": "game:" + jwt,
		"равен JWT":             jwt,
		"дев-заглушка":          "dev-only-change-me",
	}
	for name, secret := range cases {
		cfg := base
		cfg.GameAPISecret = secret
		if err := cfg.Validate(); err == nil {
			t.Fatalf("GAME_API_SECRET (%s) должен отклоняться в проде", name)
		}
	}
}

func TestValidateRejectsWeakSiteOrderSecret(t *testing.T) {
	jwt := "a-real-32-char-random-secret-value"
	base := Config{
		AppEnv:                     "production",
		JWTSecret:                  jwt,
		AnticheatSecret:            "a-distinct-anticheat-secret-value",
		GameAPISecret:              "a-distinct-game-api-secret-value",
		DatabaseURL:                "postgres://user:pass@127.0.0.1:5432/launcher?sslmode=disable",
		DeliveryManifestSigningKey: testDeliverySigningKey,
	}

	for name, secret := range map[string]string{
		"пуст":            "",
		"равен JWT":       jwt,
		"равен game":      base.GameAPISecret,
		"равен anticheat": base.AnticheatSecret,
		"дев-заглушка":    "dev-only-change-me",
	} {
		cfg := base
		cfg.SiteOrderSecret = secret
		if err := cfg.Validate(); err == nil {
			t.Fatalf("SITE_ORDER_SECRET (%s) должен отклоняться в проде", name)
		}
	}
}

func TestV1BridgeRequiresAndHonorsCutoff(t *testing.T) {
	now := time.Date(2026, 8, 21, 12, 0, 0, 0, time.UTC)
	cfg := Config{DeliveryV1Bridge: true}
	if err := cfg.Validate(); err == nil || !strings.Contains(err.Error(), "DELIVERY_V1_BRIDGE_UNTIL") {
		t.Fatalf("missing cutoff error = %v", err)
	}
	cfg.DeliveryV1BridgeUntil = now.Add(time.Hour)
	if !cfg.V1BridgeEnabled(now) {
		t.Fatal("bridge disabled before cutoff")
	}
	if cfg.V1BridgeEnabled(now.Add(2 * time.Hour)) {
		t.Fatal("bridge remained enabled after cutoff")
	}
}

func TestProductionRejectsMalformedDeliverySigningKey(t *testing.T) {
	cfg := Config{
		AppEnv:                     "production",
		JWTSecret:                  "a-real-jwt-secret-value",
		AnticheatSecret:            "a-distinct-anticheat-secret-value",
		GameAPISecret:              "a-distinct-game-api-secret-value",
		SiteOrderSecret:            "a-distinct-site-secret-value",
		DatabaseURL:                "postgres://user:pass@127.0.0.1:5432/launcher?sslmode=disable",
		DeliveryManifestSigningKey: strings.Repeat("z", 64),
	}
	if err := cfg.Validate(); err == nil || !strings.Contains(err.Error(), "DELIVERY_MANIFEST_SIGNING_KEY") {
		t.Fatalf("Validate() error = %v", err)
	}
}
