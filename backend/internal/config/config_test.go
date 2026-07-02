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
