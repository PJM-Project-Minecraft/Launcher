package config

import (
	"strings"
	"testing"
	"time"
)

const testDeliverySigningKey = "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"

func strongTestSecrets() (string, string, string, string) {
	return strings.Repeat("01", 32), strings.Repeat("2a", 32), strings.Repeat("4b", 32), strings.Repeat("6c", 32)
}

func productionConfigWithStrongSecrets() Config {
	jwtSecret, anticheatSecret, gameSecret, siteSecret := strongTestSecrets()
	return Config{
		AppEnv:                     "production",
		JWTSecret:                  jwtSecret,
		AnticheatSecret:            anticheatSecret,
		GameAPISecret:              gameSecret,
		SiteOrderSecret:            siteSecret,
		DatabaseURL:                "postgres://user:pass@127.0.0.1:5432/launcher?sslmode=disable",
		DeliveryManifestSigningKey: testDeliverySigningKey,
	}
}

func TestValidateProductionSecretsRequireCanonical256BitHex(t *testing.T) {
	valid := productionConfigWithStrongSecrets()
	if err := valid.Validate(); err != nil {
		t.Fatalf("strong production secrets rejected: %v", err)
	}

	tests := map[string]func(*Config){
		"short":          func(c *Config) { c.JWTSecret = strings.Repeat("01", 31) + "0" },
		"uppercase":      func(c *Config) { c.AnticheatSecret = strings.ToUpper(c.AnticheatSecret) },
		"non hex":        func(c *Config) { c.GameAPISecret = strings.Repeat("gh", 32) },
		"whitespace":     func(c *Config) { c.SiteOrderSecret = " " + c.SiteOrderSecret[:63] },
		"single nibble":  func(c *Config) { c.JWTSecret = strings.Repeat("a", 64) },
		"pairwise reuse": func(c *Config) { c.SiteOrderSecret = c.GameAPISecret },
	}
	for name, mutate := range tests {
		t.Run(name, func(t *testing.T) {
			cfg := valid
			mutate(&cfg)
			if err := cfg.Validate(); err == nil {
				t.Fatal("unsafe production secret configuration accepted")
			}
		})
	}
}

func TestValidateP5EnforcementRequiresSecretInProduction(t *testing.T) {
	cfg := productionConfigWithStrongSecrets()
	cfg.AnticheatP5Enforce = true
	if err := cfg.Validate(); err == nil || !strings.Contains(err.Error(), "ANTICHEAT_P5_SECRET") {
		t.Fatalf("enforcement without P5 secret accepted: %v", err)
	}
	cfg.AnticheatP5Secret = strings.Repeat("8d", 32)
	if err := cfg.Validate(); err != nil {
		t.Fatalf("valid enforced P5 config rejected: %v", err)
	}
}

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

func TestValidateDeliveryCDNConfiguration(t *testing.T) {
	validSecret := strings.Repeat("a", 64)
	for name, cfg := range map[string]Config{
		"base without origin secret": {
			AppEnv: "development", JWTSecret: "dev-only-change-me",
			DeliveryCDNBase: "https://cdn.example.com",
		},
		"origin secret without base": {
			AppEnv: "development", JWTSecret: "dev-only-change-me",
			DeliveryCDNOriginSecret: validSecret,
		},
		"insecure base": {
			AppEnv: "development", JWTSecret: "dev-only-change-me",
			DeliveryCDNBase: "http://cdn.example.com", DeliveryCDNOriginSecret: validSecret,
		},
		"weak origin secret": {
			AppEnv: "development", JWTSecret: "dev-only-change-me",
			DeliveryCDNBase: "https://cdn.example.com", DeliveryCDNOriginSecret: "short",
		},
	} {
		t.Run(name, func(t *testing.T) {
			if err := cfg.Validate(); err == nil || !strings.Contains(err.Error(), "DELIVERY_CDN") {
				t.Fatalf("Validate() error = %v", err)
			}
		})
	}

	valid := Config{
		AppEnv: "development", JWTSecret: "dev-only-change-me",
		DeliveryCDNBase: "https://cdn.example.com", DeliveryCDNOriginSecret: validSecret,
	}
	if err := valid.Validate(); err != nil {
		t.Fatalf("valid CDN config rejected: %v", err)
	}
}

func TestValidateAllowsRealSecretInProduction(t *testing.T) {
	cfg := productionConfigWithStrongSecrets()
	if err := cfg.Validate(); err != nil {
		t.Fatalf("production с нормальными секретами должен проходить: %v", err)
	}
}

func TestValidateRejectsEmptyDatabaseURLInProduction(t *testing.T) {
	cfg := productionConfigWithStrongSecrets()
	cfg.DatabaseURL = "" // Пустой URL → тихий SQLite-fallback, запрещён в проде.
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
	base := productionConfigWithStrongSecrets()
	jwt := base.JWTSecret

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
	base := productionConfigWithStrongSecrets()
	jwt := base.JWTSecret

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
	base := productionConfigWithStrongSecrets()
	jwt := base.JWTSecret

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
	cfg := productionConfigWithStrongSecrets()
	cfg.DeliveryManifestSigningKey = strings.Repeat("z", 64)
	if err := cfg.Validate(); err == nil || !strings.Contains(err.Error(), "DELIVERY_MANIFEST_SIGNING_KEY") {
		t.Fatalf("Validate() error = %v", err)
	}
}

func TestBotValidationDoesNotRequireDeliverySigningMaterial(t *testing.T) {
	cfg := productionConfigWithStrongSecrets()
	cfg.DeliveryV1Bridge = true
	cfg.DeliveryManifestSigningKey = "not-a-key"
	if err := cfg.ValidateBot(); err != nil {
		t.Fatalf("bot must not require delivery signing configuration: %v", err)
	}
}
