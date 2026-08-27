#!/usr/bin/env bash
#
# Build and install the backend as a systemd service on a VPS.

set -euo pipefail

ROOT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
BACKEND_DIR="$ROOT_DIR/backend"
SERVICE_NAME="${SERVICE_NAME:-project-minecraft-backend}"
SERVICE_USER="${SUDO_USER:-$USER}"
LISTEN_ADDR="${SERVER_ADDR:-127.0.0.1:8080}"
PUBLIC_URL="${PUBLIC_BASE_URL:-}"
AUTH_PROVIDER_URL="${AUTH_PROVIDER_URL:-https://pjm.likonchik.xyz/api/gml/auth}"
ADMIN_LOGINS="${ADMIN_LOGINS:-}"
AUTHLIB_INJECTOR_PATH="${AUTHLIB_INJECTOR_PATH:-$BACKEND_DIR/data/authlib-injector.jar}"
PROFILE_STORAGE_ROOT="${PROFILE_STORAGE_ROOT:-storage/profiles}"
ENV_FILE="$BACKEND_DIR/.env.production"
INSTALL_SERVICE=1

usage() {
  cat <<'EOF'
Usage: scripts/prod/vps-install-backend.sh --public-url https://launcher.example.com --admin-logins nick1,nick2 [options]

Options:
  --public-url URL        Public HTTPS URL of the backend
  --admin-logins LIST     Comma-separated admin logins
  --auth-provider-url URL Upstream GML auth provider
  --listen ADDR           Backend listen address (default: 127.0.0.1:8080)
  --authlib-jar PATH      authlib-injector jar exposed to launchers
  --service-name NAME     systemd service name
  --no-service            Build binary and env only, do not install systemd service
  -h, --help              Show this help

Run this on the VPS from the repository root after cloning/updating the project.
EOF
}

while [[ $# -gt 0 ]]; do
  case "$1" in
    --public-url)
      PUBLIC_URL="${2:-}"
      shift
      ;;
    --admin-logins)
      ADMIN_LOGINS="${2:-}"
      shift
      ;;
    --auth-provider-url)
      AUTH_PROVIDER_URL="${2:-}"
      shift
      ;;
    --listen)
      LISTEN_ADDR="${2:-}"
      shift
      ;;
    --authlib-jar)
      AUTHLIB_INJECTOR_PATH="${2:-}"
      shift
      ;;
    --service-name)
      SERVICE_NAME="${2:-}"
      shift
      ;;
    --no-service)
      INSTALL_SERVICE=0
      ;;
    -h|--help)
      usage
      exit 0
      ;;
    *)
      echo "Unknown option: $1" >&2
      usage >&2
      exit 2
      ;;
  esac
  shift
done

if [[ -z "$PUBLIC_URL" || "$PUBLIC_URL" != https://* ]]; then
  echo "ERROR: --public-url must be a public HTTPS URL, for example https://launcher.example.com" >&2
  exit 2
fi

if [[ -z "$ADMIN_LOGINS" ]]; then
  echo "ERROR: --admin-logins is required in production." >&2
  exit 2
fi

if ! command -v go >/dev/null 2>&1; then
  for candidate in "$HOME/.local/toolchains/go/bin/go" "/usr/local/go/bin/go"; do
    if [[ -x "$candidate" ]]; then
      export PATH="$(dirname "$candidate"):$PATH"
      break
    fi
  done
fi

if ! command -v go >/dev/null 2>&1; then
  echo "ERROR: go not found. Install Go matching backend/go.mod." >&2
  exit 1
fi

if [[ ! -f "$AUTHLIB_INJECTOR_PATH" ]]; then
  echo "ERROR: authlib-injector jar not found: $AUTHLIB_INJECTOR_PATH" >&2
  echo "Put the jar there or pass --authlib-jar /path/to/authlib-injector.jar." >&2
  exit 1
fi

random_secret() {
  if command -v openssl >/dev/null 2>&1; then
    openssl rand -hex 32
  else
    dd if=/dev/urandom bs=32 count=1 2>/dev/null | od -An -tx1 | tr -d ' \n'
    echo
  fi
}

mkdir -p "$BACKEND_DIR/bin" "$BACKEND_DIR/data" "$BACKEND_DIR/storage/profiles"

JWT_SECRET="$(random_secret)"
ANTICHEAT_SECRET="$(random_secret)"
GAME_API_SECRET="$(random_secret)"
SITE_ORDER_SECRET="$(random_secret)"
if [[ -f "$ENV_FILE" ]]; then
  BACKUP="$ENV_FILE.$(date +%Y%m%d-%H%M%S).bak"
  cp "$ENV_FILE" "$BACKUP"
  echo "[backend] Existing env backed up to $BACKUP"
  for secret_name in JWT_SECRET ANTICHEAT_SECRET GAME_API_SECRET SITE_ORDER_SECRET; do
    if grep -q "^${secret_name}=" "$BACKUP"; then
      printf -v "$secret_name" '%s' "$(grep "^${secret_name}=" "$BACKUP" | tail -n1 | cut -d= -f2-)"
    fi
  done
fi

if [[ -z "${DATABASE_URL:-}" ]]; then
  echo "ERROR: production install requires DATABASE_URL for PostgreSQL." >&2
  exit 1
fi

cat > "$ENV_FILE" <<EOF
APP_ENV=production
SERVER_ADDR=$LISTEN_ADDR
PUBLIC_BASE_URL=${PUBLIC_URL%/}
AUTH_PROVIDER_URL=$AUTH_PROVIDER_URL
JWT_SECRET=$JWT_SECRET
ANTICHEAT_SECRET=$ANTICHEAT_SECRET
GAME_API_SECRET=$GAME_API_SECRET
SITE_ORDER_SECRET=$SITE_ORDER_SECRET
ALLOWED_ORIGINS=${PUBLIC_URL%/},http://127.0.0.1:3000,http://localhost:3000
ADMIN_LOGINS=$ADMIN_LOGINS
PROFILE_STORAGE_ROOT=$PROFILE_STORAGE_ROOT
YGGDRASIL_KEY_PATH=data/yggdrasil_key.pem
YGGDRASIL_SERVER_NAME=Project Minecraft
AUTHLIB_INJECTOR_PATH=$AUTHLIB_INJECTOR_PATH

# Production is PostgreSQL-only; Config.Validate rejects a SQLite fallback.
DATABASE_URL=$DATABASE_URL
EOF
chmod 600 "$ENV_FILE"

echo "[backend] Building backend binary"
(
  cd "$BACKEND_DIR"
  go build -o bin/launcher-backend ./cmd/server
)

if (( INSTALL_SERVICE == 0 )); then
  echo "[backend] Built: $BACKEND_DIR/bin/launcher-backend"
  echo "[backend] Env:   $ENV_FILE"
  exit 0
fi

if ! command -v sudo >/dev/null 2>&1; then
  echo "ERROR: sudo not found. Re-run with --no-service or install systemd service manually." >&2
  exit 1
fi

UNIT_PATH="/etc/systemd/system/$SERVICE_NAME.service"
TMP_UNIT="$(mktemp)"
cat > "$TMP_UNIT" <<EOF
[Unit]
Description=Project Minecraft Backend
After=network-online.target
Wants=network-online.target

[Service]
Type=simple
User=$SERVICE_USER
WorkingDirectory=$BACKEND_DIR
EnvironmentFile=$ENV_FILE
ExecStart=$BACKEND_DIR/bin/launcher-backend
Restart=always
RestartSec=3
NoNewPrivileges=true
PrivateTmp=true
ProtectSystem=full
ReadWritePaths=$BACKEND_DIR/data $BACKEND_DIR/storage

[Install]
WantedBy=multi-user.target
EOF

echo "[backend] Installing systemd unit: $UNIT_PATH"
sudo install -m 0644 "$TMP_UNIT" "$UNIT_PATH"
rm -f "$TMP_UNIT"
sudo systemctl daemon-reload
sudo systemctl enable --now "$SERVICE_NAME"
sudo systemctl restart "$SERVICE_NAME"

echo ""
echo "[backend] Installed and started."
echo "Status:"
sudo systemctl --no-pager --full status "$SERVICE_NAME" || true
echo ""
echo "Health:"
echo "  curl ${PUBLIC_URL%/}/api/yggdrasil/"
