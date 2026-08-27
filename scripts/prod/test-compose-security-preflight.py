#!/usr/bin/env python3
import importlib.util
import pathlib
import unittest


SCRIPT = pathlib.Path(__file__).with_name("compose-security-preflight.py")


def load_preflight():
    spec = importlib.util.spec_from_file_location("compose_security_preflight", SCRIPT)
    module = importlib.util.module_from_spec(spec)
    spec.loader.exec_module(module)
    return module


def valid_model():
    secrets = {
        "JWT_SECRET": "01" * 32,
        "ANTICHEAT_SECRET": "2a" * 32,
        "GAME_API_SECRET": "4b" * 32,
        "SITE_ORDER_SECRET": "6c" * 32,
    }
    return {
        "services": {
            "postgres": {
                "ports": [{"target": 5432, "published": "5432", "host_ip": "127.0.0.1"}],
            },
            "server": {
                "environment": {
                    "APP_ENV": "production",
                    "DATABASE_URL": "postgres://launcher:password@postgres:5432/launcher?sslmode=disable",
                    **secrets,
                },
                "ports": [{"target": 8080, "published": "8080", "host_ip": "127.0.0.1"}],
                "volumes": [{"type": "volume", "source": "launcher_storage", "target": "/app/storage"}],
            },
            "bot": {"environment": {"APP_ENV": "production", **secrets}},
        }
    }


class ComposeSecurityPreflightTests(unittest.TestCase):
    @classmethod
    def setUpClass(cls):
        cls.preflight = load_preflight()

    def test_accepts_valid_rendered_production_model(self):
        self.assertEqual(self.preflight.validate(valid_model()), [])

    def test_rejects_weak_or_reused_secret_without_echoing_it(self):
        model = valid_model()
        model["services"]["server"]["environment"]["JWT_SECRET"] = "exposed-secret"
        model["services"]["server"]["environment"]["SITE_ORDER_SECRET"] = "4b" * 32
        errors = self.preflight.validate(model)
        self.assertTrue(any("JWT_SECRET" in error for error in errors))
        self.assertTrue(any("distinct" in error for error in errors))
        self.assertNotIn("exposed-secret", "\n".join(errors))

    def test_rejects_development_missing_storage_and_wildcard_binds(self):
        model = valid_model()
        server = model["services"]["server"]
        server["environment"]["APP_ENV"] = "development"
        server["ports"][0]["host_ip"] = "0.0.0.0"
        server["volumes"] = []
        model["services"]["postgres"]["ports"][0]["host_ip"] = "::"
        errors = "\n".join(self.preflight.validate(model))
        self.assertIn("APP_ENV", errors)
        self.assertIn("server port", errors)
        self.assertIn("postgres port", errors)
        self.assertIn("/app/storage", errors)

    def test_rejects_missing_postgres_dsn(self):
        model = valid_model()
        model["services"]["server"]["environment"]["DATABASE_URL"] = ""
        self.assertTrue(any("DATABASE_URL" in error for error in self.preflight.validate(model)))

    def test_rejects_weak_or_mismatched_optional_p5_secret(self):
        model = valid_model()
        model["services"]["server"]["environment"]["ANTICHEAT_P5_SECRET"] = "weak"
        errors = "\n".join(self.preflight.validate(model))
        self.assertIn("ANTICHEAT_P5_SECRET must be 32 random bytes", errors)
        self.assertNotIn("weak", errors)

    def test_rejects_p5_enforcement_without_secret(self):
        model = valid_model()
        model["services"]["server"]["environment"]["ANTICHEAT_P5_ENFORCE"] = "true"
        errors = "\n".join(self.preflight.validate(model))
        self.assertIn("ANTICHEAT_P5_ENFORCE=true requires ANTICHEAT_P5_SECRET", errors)


if __name__ == "__main__":
    unittest.main()
