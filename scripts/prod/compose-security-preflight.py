#!/usr/bin/env python3
"""Validate a rendered production Compose model without printing secret values."""

import json
import re
import sys


SECRET_NAMES = ("JWT_SECRET", "ANTICHEAT_SECRET", "GAME_API_SECRET", "SITE_ORDER_SECRET")
OPTIONAL_SECRET_NAMES = ("ANTICHEAT_P5_SECRET",)
SECRET_RE = re.compile(r"[0-9a-f]{64}\Z")
LOOPBACKS = {"127.0.0.1", "::1"}


def environment(service):
    raw = service.get("environment") or {}
    if isinstance(raw, dict):
        return {str(key): "" if value is None else str(value) for key, value in raw.items()}
    result = {}
    for item in raw:
        key, _, value = str(item).partition("=")
        result[key] = value
    return result


def port_is_loopback(port):
    if isinstance(port, dict):
        return str(port.get("host_ip") or "") in LOOPBACKS
    value = str(port)
    return value.startswith("127.0.0.1:") or value.startswith("[::1]:")


def has_storage_mount(service):
    for volume in service.get("volumes") or []:
        if isinstance(volume, dict):
            if volume.get("target") == "/app/storage" and volume.get("type") in {"volume", "bind"}:
                return True
        elif str(volume).rsplit(":", 1)[-1] == "/app/storage":
            return True
    return False


def validate(model):
    errors = []
    services = model.get("services") or {}
    server = services.get("server") or {}
    bot = services.get("bot") or {}
    postgres = services.get("postgres") or {}
    server_env = environment(server)
    bot_env = environment(bot)

    for service_name, env in (("server", server_env), ("bot", bot_env)):
        if env.get("APP_ENV") != "production":
            errors.append(f"{service_name}: APP_ENV must be production")

    values = []
    for name in SECRET_NAMES:
        value = server_env.get(name, "")
        if not SECRET_RE.fullmatch(value) or len(set(value)) == 1:
            errors.append(f"server: {name} must be 32 random bytes in lowercase hex")
        values.append(value)
        if bot_env.get(name) != value:
            errors.append(f"bot: {name} must match the validated server value")
    for name in OPTIONAL_SECRET_NAMES:
        value = server_env.get(name, "")
        if value and (not SECRET_RE.fullmatch(value) or len(set(value)) == 1):
            errors.append(f"server: {name} must be 32 random bytes in lowercase hex when configured")
        if value:
            values.append(value)
    if server_env.get("ANTICHEAT_P5_ENFORCE") == "true" and not server_env.get("ANTICHEAT_P5_SECRET"):
        errors.append("server: ANTICHEAT_P5_ENFORCE=true requires ANTICHEAT_P5_SECRET")
    nonempty_values = [value for value in values if value]
    if len(set(nonempty_values)) != len(nonempty_values):
        errors.append("server: production secrets must be pairwise distinct")

    dsn = server_env.get("DATABASE_URL", "")
    if not (dsn.startswith("postgres://") or dsn.startswith("postgresql://")):
        errors.append("server: DATABASE_URL must be an explicit PostgreSQL DSN")

    for service_name, service in (("postgres", postgres), ("server", server)):
        ports = service.get("ports") or []
        if any(not port_is_loopback(port) for port in ports):
            errors.append(f"{service_name} port must publish only on loopback")

    if not has_storage_mount(server):
        errors.append("server: persistent /app/storage mount is required")
    return errors


def main():
    try:
        if len(sys.argv) > 1:
            with open(sys.argv[1], "r", encoding="utf-8") as source:
                model = json.load(source)
        else:
            model = json.load(sys.stdin)
    except (OSError, json.JSONDecodeError) as error:
        print(f"compose preflight: invalid rendered JSON ({type(error).__name__})", file=sys.stderr)
        return 2

    errors = validate(model)
    for error in errors:
        print(f"compose preflight: {error}", file=sys.stderr)
    return 1 if errors else 0


if __name__ == "__main__":
    raise SystemExit(main())
