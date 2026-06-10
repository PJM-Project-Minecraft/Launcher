#!/usr/bin/env bash
#
# dev.sh — Launch the full Launcher development stack.
#
# Components:
#   • Docker   — PostgreSQL, Redis, MinIO (if Docker is available)
#   • Backend  — Go + Fiber (backend/cmd/server)
#   • Dashboard — Next.js  (dashboard/)
#
# If Docker is not installed, the backend will use SQLite instead of PostgreSQL.
# Press Ctrl+C to gracefully stop everything.

set -euo pipefail

# ─── Project root ──────────────────────────────────────────────────────────────
SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
cd "$SCRIPT_DIR"

# ─── Colors & formatting ──────────────────────────────────────────────────────
RESET="\033[0m"
BOLD="\033[1m"
DIM="\033[2m"

# Service colors
COLOR_DOCKER="\033[38;5;39m"    # blue
COLOR_POSTGRES="\033[38;5;33m"  # dark blue
COLOR_REDIS="\033[38;5;196m"    # red
COLOR_MINIO="\033[38;5;208m"    # orange
COLOR_BACKEND="\033[38;5;76m"   # green
COLOR_BOT="\033[38;5;45m"       # cyan
COLOR_DASHBOARD="\033[38;5;213m" # pink
COLOR_SYSTEM="\033[38;5;245m"   # gray
COLOR_ERROR="\033[38;5;196m"    # red
COLOR_SUCCESS="\033[38;5;82m"   # bright green
COLOR_WARN="\033[38;5;220m"     # yellow

# ─── PIDs to track ─────────────────────────────────────────────────────────────
PIDS=()
DOCKER_UP=false
HAS_DOCKER=false

# ─── Logging helpers ───────────────────────────────────────────────────────────
log() {
  local color="$1" label="$2"
  shift 2
  echo -e "${color}${BOLD}[${label}]${RESET} $*"
}

log_prefixed() {
  local color="$1" label="$2"
  shift 2
  while IFS= read -r line || [[ -n "$line" ]]; do
    echo -e "${color}${BOLD}[${label}]${RESET} ${line}"
  done
}

banner() {
  echo ""
  echo -e "${BOLD}${COLOR_SYSTEM}╔══════════════════════════════════════════════════════════════╗${RESET}"
  echo -e "${BOLD}${COLOR_SYSTEM}║${RESET}  ${BOLD}🚀 Project Minecraft — Development Environment${RESET}            ${BOLD}${COLOR_SYSTEM}║${RESET}"
  echo -e "${BOLD}${COLOR_SYSTEM}╠══════════════════════════════════════════════════════════════╣${RESET}"
  if $HAS_DOCKER; then
    echo -e "${BOLD}${COLOR_SYSTEM}║${RESET}  ${COLOR_DOCKER}■${RESET} Docker     ${DIM}PostgreSQL · Redis · MinIO${RESET}                   ${BOLD}${COLOR_SYSTEM}║${RESET}"
  else
    echo -e "${BOLD}${COLOR_SYSTEM}║${RESET}  ${COLOR_WARN}■${RESET} Database   ${DIM}SQLite (no Docker)${RESET}                            ${BOLD}${COLOR_SYSTEM}║${RESET}"
  fi
  echo -e "${BOLD}${COLOR_SYSTEM}║${RESET}  ${COLOR_BACKEND}■${RESET} Backend    ${DIM}Go + Fiber  → :8080${RESET}                           ${BOLD}${COLOR_SYSTEM}║${RESET}"
  echo -e "${BOLD}${COLOR_SYSTEM}║${RESET}  ${COLOR_BOT}■${RESET} Telegram   ${DIM}Bot (cmd/bot, optional)${RESET}                       ${BOLD}${COLOR_SYSTEM}║${RESET}"
  echo -e "${BOLD}${COLOR_SYSTEM}║${RESET}  ${COLOR_DASHBOARD}■${RESET} Dashboard  ${DIM}Next.js     → :3000${RESET}                           ${BOLD}${COLOR_SYSTEM}║${RESET}"
  echo -e "${BOLD}${COLOR_SYSTEM}╠══════════════════════════════════════════════════════════════╣${RESET}"
  echo -e "${BOLD}${COLOR_SYSTEM}║${RESET}  ${DIM}Press Ctrl+C to stop all services${RESET}                          ${BOLD}${COLOR_SYSTEM}║${RESET}"
  echo -e "${BOLD}${COLOR_SYSTEM}╚══════════════════════════════════════════════════════════════╝${RESET}"
  echo ""
}

# ─── Cleanup on exit ──────────────────────────────────────────────────────────
cleanup() {
  echo ""
  log "$COLOR_SYSTEM" "SYSTEM" "Shutting down..."

  # Kill tracked background processes
  for pid in "${PIDS[@]}"; do
    if kill -0 "$pid" 2>/dev/null; then
      kill -TERM "$pid" 2>/dev/null || true
    fi
  done

  # Wait briefly for processes to exit
  for pid in "${PIDS[@]}"; do
    if kill -0 "$pid" 2>/dev/null; then
      wait "$pid" 2>/dev/null || true
    fi
  done

  # Stop Docker containers
  if $DOCKER_UP; then
    log "$COLOR_DOCKER" "DOCKER" "Stopping containers..."
    docker compose down --timeout 5 2>&1 | log_prefixed "$COLOR_DOCKER" "DOCKER"
  fi

  log "$COLOR_SUCCESS" "SYSTEM" "All services stopped. Goodbye! 👋"
  exit 0
}

trap cleanup SIGINT SIGTERM EXIT

# ─── Dependency checks ────────────────────────────────────────────────────────
check_deps() {
  local missing=()

  # Check for Docker (optional)
  if command -v docker &>/dev/null && docker info &>/dev/null 2>&1; then
    HAS_DOCKER=true
  else
    HAS_DOCKER=false
    log "$COLOR_WARN" "SYSTEM" "Docker not available — using SQLite fallback"
  fi

  command -v node    &>/dev/null || missing+=("node")
  command -v npm     &>/dev/null || missing+=("npm")

  # Source Go toolchain if needed
  if ! command -v go &>/dev/null; then
    if [[ -f "$HOME/.local/toolchains/go/bin/go" ]]; then
      export PATH="$HOME/.local/toolchains/go/bin:$PATH"
    else
      missing+=("go")
    fi
  fi

  if (( ${#missing[@]} > 0 )); then
    log "$COLOR_ERROR" "ERROR" "Missing dependencies: ${missing[*]}"
    log "$COLOR_ERROR" "ERROR" "Please install them before running this script."
    exit 1
  fi
}

# ─── Wait for a TCP port ──────────────────────────────────────────────────────
wait_for_port() {
  local label="$1" host="$2" port="$3" timeout="${4:-30}" color="$5"
  local elapsed=0
  log "$color" "$label" "Waiting for ${host}:${port}..."
  while ! (echo >/dev/tcp/"$host"/"$port") 2>/dev/null; do
    sleep 1
    elapsed=$((elapsed + 1))
    if (( elapsed >= timeout )); then
      log "$COLOR_ERROR" "$label" "Timed out waiting for ${host}:${port}"
      return 1
    fi
  done
  log "$COLOR_SUCCESS" "$label" "Ready on ${host}:${port} ✓"
}

# ═══════════════════════════════════════════════════════════════════════════════
# MAIN
# ═══════════════════════════════════════════════════════════════════════════════

check_deps
banner

# ─── 1. Docker services (if available) ─────────────────────────────────────────
if $HAS_DOCKER; then
  log "$COLOR_DOCKER" "DOCKER" "Starting infrastructure containers..."
  docker compose up -d 2>&1 | log_prefixed "$COLOR_DOCKER" "DOCKER"
  DOCKER_UP=true

  # Follow Docker logs in background
  docker compose logs -f --tail=20 2>&1 | while IFS= read -r line; do
    case "$line" in
      *postgres*) echo -e "${COLOR_POSTGRES}${BOLD}[POSTGRES]${RESET} ${line}" ;;
      *)          echo -e "${COLOR_DOCKER}${BOLD}[DOCKER]${RESET}   ${line}" ;;
    esac
  done &
  PIDS+=($!)

  # Wait for services to be ready
  wait_for_port "POSTGRES" "127.0.0.1" 5432 30 "$COLOR_POSTGRES"

  echo ""
  log "$COLOR_SUCCESS" "DOCKER" "All infrastructure services are up ✓"
  echo ""
else
  log "$COLOR_WARN" "SYSTEM" "Skipping Docker — backend will use SQLite"
  echo ""
fi

# ─── 2. Backend ────────────────────────────────────────────────────────────────
log "$COLOR_BACKEND" "BACKEND" "Starting Go backend server..."
(
  cd "$SCRIPT_DIR/backend"
  export SERVER_ADDR="127.0.0.1:8080"
  export AUTH_PROVIDER_URL="https://pjm.likonchik.xyz/api/gml/auth"
  export JWT_SECRET="dev-local-launcher-secret"
  export ALLOWED_ORIGINS="http://127.0.0.1:5173,http://localhost:5173,http://127.0.0.1:3000,http://localhost:3000"
  export APP_ENV="development"
  export AUTH_MODE="${AUTH_MODE:-local}"

  if $HAS_DOCKER; then
    export DATABASE_URL="postgres://launcher:launcher_dev_password@127.0.0.1:5432/launcher?sslmode=disable"
  fi
  # If DATABASE_URL is not set, backend falls back to SQLite automatically

  go run ./cmd/server 2>&1 | log_prefixed "$COLOR_BACKEND" "BACKEND"
) &
PIDS+=($!)

# Give backend a moment to start
sleep 2
wait_for_port "BACKEND" "127.0.0.1" 8080 20 "$COLOR_BACKEND"

# ─── 2b. Telegram bot (optional) ──────────────────────────────────────────────
# Запускается только если задан TELEGRAM_BOT_TOKEN (в окружении или backend/.env).
BOT_TOKEN_PRESENT=false
if [[ -n "${TELEGRAM_BOT_TOKEN:-}" ]]; then
  BOT_TOKEN_PRESENT=true
elif [[ -f "$SCRIPT_DIR/backend/.env" ]] && grep -qE '^\s*TELEGRAM_BOT_TOKEN\s*=\s*\S' "$SCRIPT_DIR/backend/.env"; then
  BOT_TOKEN_PRESENT=true
fi

if $BOT_TOKEN_PRESENT; then
  log "$COLOR_BOT" "BOT" "Starting Telegram account bot..."
  (
    cd "$SCRIPT_DIR/backend"
    export AUTH_MODE="${AUTH_MODE:-local}"
    if $HAS_DOCKER; then
      export DATABASE_URL="postgres://launcher:launcher_dev_password@127.0.0.1:5432/launcher?sslmode=disable"
    fi
    go run ./cmd/bot 2>&1 | log_prefixed "$COLOR_BOT" "BOT"
  ) &
  PIDS+=($!)
else
  log "$COLOR_WARN" "BOT" "TELEGRAM_BOT_TOKEN не задан — Telegram-бот пропущен (см. backend/.env)"
fi

# ─── 3. Dashboard ─────────────────────────────────────────────────────────────
log "$COLOR_DASHBOARD" "DASHBOARD" "Starting Next.js dashboard..."

# Install deps if needed
if [[ ! -d "$SCRIPT_DIR/dashboard/node_modules" ]]; then
  log "$COLOR_DASHBOARD" "DASHBOARD" "Installing dependencies..."
  npm --prefix "$SCRIPT_DIR/dashboard" install 2>&1 | log_prefixed "$COLOR_DASHBOARD" "DASHBOARD"
fi

(
  cd "$SCRIPT_DIR/dashboard"
  export NEXT_PUBLIC_API_URL="http://127.0.0.1:8080"
  npx next dev 2>&1 | log_prefixed "$COLOR_DASHBOARD" "DASHBOARD"
) &
PIDS+=($!)

# ─── Status summary ───────────────────────────────────────────────────────────
sleep 3
echo ""
echo -e "${BOLD}${COLOR_SYSTEM}┌──────────────────────────────────────────────────────────────┐${RESET}"
echo -e "${BOLD}${COLOR_SYSTEM}│${RESET}  ${COLOR_SUCCESS}${BOLD}✓ All services are running!${RESET}                                 ${BOLD}${COLOR_SYSTEM}│${RESET}"
echo -e "${BOLD}${COLOR_SYSTEM}├──────────────────────────────────────────────────────────────┤${RESET}"
if $HAS_DOCKER; then
  echo -e "${BOLD}${COLOR_SYSTEM}│${RESET}  ${COLOR_POSTGRES}PostgreSQL${RESET}   →  127.0.0.1:${BOLD}5432${RESET}                          ${BOLD}${COLOR_SYSTEM}│${RESET}"
else
  echo -e "${BOLD}${COLOR_SYSTEM}│${RESET}  ${COLOR_WARN}SQLite${RESET}       →  backend/data/launcher.db                  ${BOLD}${COLOR_SYSTEM}│${RESET}"
fi
echo -e "${BOLD}${COLOR_SYSTEM}│${RESET}  ${COLOR_BACKEND}Backend${RESET}      →  127.0.0.1:${BOLD}8080${RESET}  ${DIM}(AUTH_MODE=${AUTH_MODE:-local})${RESET}         ${BOLD}${COLOR_SYSTEM}│${RESET}"
if $BOT_TOKEN_PRESENT; then
  echo -e "${BOLD}${COLOR_SYSTEM}│${RESET}  ${COLOR_BOT}Telegram bot${RESET} →  ${BOLD}polling${RESET} ${DIM}(общая БД)${RESET}                          ${BOLD}${COLOR_SYSTEM}│${RESET}"
else
  echo -e "${BOLD}${COLOR_SYSTEM}│${RESET}  ${COLOR_WARN}Telegram bot${RESET} →  ${DIM}пропущен (нет TELEGRAM_BOT_TOKEN)${RESET}          ${BOLD}${COLOR_SYSTEM}│${RESET}"
fi
echo -e "${BOLD}${COLOR_SYSTEM}│${RESET}  ${COLOR_DASHBOARD}Dashboard${RESET}    →  127.0.0.1:${BOLD}3000${RESET}                          ${BOLD}${COLOR_SYSTEM}│${RESET}"
echo -e "${BOLD}${COLOR_SYSTEM}└──────────────────────────────────────────────────────────────┘${RESET}"
echo ""

# ─── Wait forever (until Ctrl+C) ──────────────────────────────────────────────
wait
