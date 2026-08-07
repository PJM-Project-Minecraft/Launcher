package config

import "testing"

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
		AppEnv:          "production",
		JWTSecret:       "a-real-32-char-random-secret-value",
		AnticheatSecret: "a-distinct-anticheat-secret-value",
		DatabaseURL:     "postgres://user:pass@127.0.0.1:5432/launcher?sslmode=disable",
	}
	if err := cfg.Validate(); err != nil {
		t.Fatalf("production с нормальными секретами должен проходить: %v", err)
	}
}

func TestValidateRejectsEmptyDatabaseURLInProduction(t *testing.T) {
	cfg := Config{
		AppEnv:          "production",
		JWTSecret:       "a-real-32-char-random-secret-value",
		AnticheatSecret: "a-distinct-anticheat-secret-value",
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
	base := Config{AppEnv: "production", JWTSecret: jwt}

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
		AppEnv:          "production",
		JWTSecret:       jwt,
		AnticheatSecret: "a-distinct-anticheat-secret-value",
		DatabaseURL:     "postgres://user:pass@127.0.0.1:5432/launcher?sslmode=disable",
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
