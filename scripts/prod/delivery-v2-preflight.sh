#!/usr/bin/env bash
# Fail-closed production gate for delivery v2. Run on the VPS after the new
# image is started, but before delivery-migrate or launcher publication.

set -euo pipefail

PROJECT_DIR="${LAUNCHER_DIR:-/root/Launcher}"
BACKUP_DIR="${BACKUP_DIR:-/root/backups/launcher}"
MAX_BACKUP_AGE_HOURS="${MAX_BACKUP_AGE_HOURS:-24}"
MIN_RESERVE_BYTES="${MIN_RESERVE_BYTES:-4294967296}"
VERIFY_ONLY=0

usage() {
  cat <<'EOF'
Usage: scripts/prod/delivery-v2-preflight.sh [--project-dir DIR] [--verify-only]

Default mode validates compose, secrets, persistent storage, a recent backup,
disk capacity and all legacy migration sources. It never migrates or runs GC.

--verify-only additionally reconstructs active v2 releases from CAS after an
operator has completed delivery-migrate.
EOF
}

while [[ $# -gt 0 ]]; do
  case "$1" in
    --project-dir)
      PROJECT_DIR="${2:-}"
      shift
      ;;
    --verify-only)
      VERIFY_ONLY=1
      ;;
    -h|--help)
      usage
      exit 0
      ;;
    *)
      echo "ERROR: unknown option: $1" >&2
      usage >&2
      exit 2
      ;;
  esac
  shift
done

fail() { echo "ERROR: $*" >&2; exit 1; }
ok() { echo "OK: $*"; }

[[ -d "$PROJECT_DIR" ]] || fail "project directory is missing: $PROJECT_DIR"
cd "$PROJECT_DIR"
docker compose config -q
ok "docker compose config"

server_id="$(docker compose ps -q server)"
[[ -n "$server_id" ]] || fail "server container is not running"
[[ "$(docker inspect -f '{{.State.Status}}' "$server_id")" == "running" ]] || fail "server container is not running"

container_env="$(docker inspect -f '{{range .Config.Env}}{{println .}}{{end}}' "$server_id")"
env_value() {
  local key="$1"
  sed -n "s/^${key}=//p" <<<"$container_env" | tail -1
}

[[ "$(env_value APP_ENV)" == "production" ]] || fail "APP_ENV must be production"
manifest_seed="$(env_value DELIVERY_MANIFEST_SIGNING_KEY)"
[[ "$manifest_seed" =~ ^[0-9a-f]{64}$ ]] || fail "DELIVERY_MANIFEST_SIGNING_KEY is missing or malformed"
unset manifest_seed
[[ "$(env_value DELIVERY_V1_BRIDGE)" == "true" ]] || fail "DELIVERY_V1_BRIDGE must be true for the migration release"
bridge_until="$(env_value DELIVERY_V1_BRIDGE_UNTIL)"
[[ -n "$bridge_until" ]] || fail "DELIVERY_V1_BRIDGE_UNTIL is missing"
bridge_epoch="$(date -u -d "$bridge_until" +%s 2>/dev/null)" || fail "DELIVERY_V1_BRIDGE_UNTIL is not RFC3339"
now_epoch="$(date -u +%s)"
(( bridge_epoch > now_epoch + 86400 )) || fail "v1 bridge cutoff must be at least 24 hours in the future"
(( bridge_epoch <= now_epoch + 45*86400 )) || fail "v1 bridge cutoff must be within 45 days"
ok "production delivery configuration (secret values hidden)"

for binary in /app/delivery-migrate /app/delivery-gc; do
  docker compose exec -T server test -x "$binary" || fail "operator binary is missing: $binary"
done
ok "operator binaries"

storage_source="$(docker inspect -f '{{range .Mounts}}{{if eq .Destination "/app/storage"}}{{.Source}}{{end}}{{end}}' "$server_id")"
[[ -n "$storage_source" && -d "$storage_source" ]] || fail "/app/storage is not backed by a persistent host mount"
[[ -w "$storage_source" ]] || fail "persistent storage is not writable: $storage_source"

latest_backup="$(ls -1t "$BACKUP_DIR"/launcher-*.sql.gz 2>/dev/null | head -1 || true)"
[[ -n "$latest_backup" && -s "$latest_backup" ]] || fail "no non-empty PostgreSQL backup found in $BACKUP_DIR"
gzip -t "$latest_backup" || fail "latest PostgreSQL backup is corrupt: $latest_backup"
backup_epoch="$(date -r "$latest_backup" +%s)"
(( now_epoch - backup_epoch <= MAX_BACKUP_AGE_HOURS*3600 )) || fail "latest PostgreSQL backup is older than ${MAX_BACKUP_AGE_HOURS}h"
ok "recent PostgreSQL backup: $(basename "$latest_backup")"

profile_bytes="$(du -sb "$storage_source/profiles" 2>/dev/null | awk '{print $1}')"
launcher_bytes="$(du -sb "$storage_source/releases" 2>/dev/null | awk '{print $1}')"
profile_bytes="${profile_bytes:-0}"
launcher_bytes="${launcher_bytes:-0}"
required_bytes=$((profile_bytes + launcher_bytes + MIN_RESERVE_BYTES))
available_bytes="$(df -PB1 "$storage_source" | awk 'NR==2 {print $4}')"
(( available_bytes >= required_bytes )) || fail "insufficient disk: available=$available_bytes required=$required_bytes"
ok "disk capacity: available=$available_bytes conservative_required=$required_bytes"

if (( VERIFY_ONLY == 1 )); then
  docker compose exec -T server /app/delivery-migrate --verify-only
  ok "published delivery v2 audit"
else
  docker compose exec -T server /app/delivery-migrate --dry-run
  ok "read-only migration rehearsal"
fi
