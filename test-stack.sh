#!/usr/bin/env bash
#
# Local gameplay test stack:
#   - backend on 127.0.0.1:8080
#   - Minecraft test server on localhost:25565
#   - Slint launcher pointed at the local backend
#
# Ctrl+C stops only the services started by this script. If a port is already
# occupied, the script reuses that existing service and leaves it alone.

set -euo pipefail

ROOT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
DEFAULT_SERVER_DIR="$HOME/Сервер для тестов"

AUTHLIB_INJECTOR_PATH_EXPLICIT="${AUTHLIB_INJECTOR_PATH+x}"
SERVER_DIR="${TEST_SERVER_DIR:-$DEFAULT_SERVER_DIR}"
SERVER_ADDR="${SERVER_ADDR:-127.0.0.1:8080}"
PUBLIC_BASE_URL="${PUBLIC_BASE_URL:-http://$SERVER_ADDR}"
AUTHLIB_INJECTOR_PATH="${AUTHLIB_INJECTOR_PATH:-$SERVER_DIR/authlib-injector-1.2.5.jar}"
MC_HOST="${MC_HOST:-127.0.0.1}"
MC_PORT="${MC_PORT:-25565}"

START_BACKEND=1
START_MC_SERVER=1
START_LAUNCHER=1
BUILD_LAUNCHER="${BUILD_LAUNCHER:-1}"

PIDS=()
LABELS=()

RESET="$(printf '\033[0m')"
BOLD="$(printf '\033[1m')"
DIM="$(printf '\033[2m')"
GREEN="$(printf '\033[38;5;76m')"
YELLOW="$(printf '\033[38;5;220m')"
BLUE="$(printf '\033[38;5;39m')"
GRAY="$(printf '\033[38;5;245m')"
RED="$(printf '\033[38;5;196m')"

usage() {
  cat <<'EOF'
Usage: ./test-stack.sh [options]

Options:
  --no-backend           Do not start the Go backend
  --no-server            Do not start the Minecraft test server
  --no-launcher          Do not start the Slint launcher
  --no-build-launcher    Do not rebuild launcher-slint release binary
  --server-dir PATH      Minecraft test server directory
  -h, --help             Show this help

Environment overrides:
  TEST_SERVER_DIR, SERVER_ADDR, PUBLIC_BASE_URL, AUTHLIB_INJECTOR_PATH,
  MC_HOST, MC_PORT, BUILD_LAUNCHER
EOF
}

while [[ $# -gt 0 ]]; do
  case "$1" in
    --no-backend)
      START_BACKEND=0
      ;;
    --no-server)
      START_MC_SERVER=0
      ;;
    --no-launcher)
      START_LAUNCHER=0
      ;;
    --no-build-launcher)
      BUILD_LAUNCHER=0
      ;;
    --server-dir)
      if [[ $# -lt 2 ]]; then
        echo "Missing value for --server-dir" >&2
        exit 2
      fi
      SERVER_DIR="$2"
      if [[ -z "$AUTHLIB_INJECTOR_PATH_EXPLICIT" ]]; then
        AUTHLIB_INJECTOR_PATH="$SERVER_DIR/authlib-injector-1.2.5.jar"
      fi
      shift
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

BACKEND_HOST="${SERVER_ADDR%:*}"
BACKEND_PORT="${SERVER_ADDR##*:}"

log() {
  local color="$1" label="$2"
  shift 2
  printf '%s%s[%s]%s %s\n' "$color" "$BOLD" "$label" "$RESET" "$*"
}

prefix_output() {
  local color="$1" label="$2"
  while IFS= read -r line || [[ -n "$line" ]]; do
    printf '%s%s[%s]%s %s\n' "$color" "$BOLD" "$label" "$RESET" "$line"
  done
}

port_open() {
  local host="$1" port="$2"
  (echo >"/dev/tcp/$host/$port") >/dev/null 2>&1
}

wait_for_port() {
  local label="$1" host="$2" port="$3" timeout="${4:-30}" color="$5"
  local elapsed=0

  log "$color" "$label" "Waiting for $host:$port..."
  until port_open "$host" "$port"; do
    sleep 1
    elapsed=$((elapsed + 1))
    if (( elapsed >= timeout )); then
      log "$RED" "$label" "Timed out waiting for $host:$port"
      return 1
    fi
  done
  log "$GREEN" "$label" "Ready on $host:$port"
}

track_pid() {
  PIDS+=("$1")
  LABELS+=("$2")
}

cleanup() {
  local code=$?
  trap - EXIT SIGINT SIGTERM

  if (( ${#PIDS[@]} > 0 )); then
    echo ""
    log "$GRAY" "SYSTEM" "Stopping local test stack..."
  fi

  for index in "${!PIDS[@]}"; do
    local pid="${PIDS[$index]}"
    local label="${LABELS[$index]}"
    if kill -0 "$pid" 2>/dev/null; then
      log "$GRAY" "SYSTEM" "Stopping $label (pid $pid)"
      kill -TERM "$pid" 2>/dev/null || true
    fi
  done

  for pid in "${PIDS[@]}"; do
    wait "$pid" 2>/dev/null || true
  done

  exit "$code"
}

trap cleanup EXIT SIGINT SIGTERM

need_cmd() {
  if command -v "$1" >/dev/null 2>&1; then
    return
  fi

  case "$1" in
    go)
      for candidate in "$HOME/.local/toolchains/go/bin/go" "/usr/local/go/bin/go"; do
        if [[ -x "$candidate" ]]; then
          export PATH="$(dirname "$candidate"):$PATH"
          return
        fi
      done
      ;;
    cargo)
      if [[ -x "$HOME/.cargo/bin/cargo" ]]; then
        export PATH="$HOME/.cargo/bin:$PATH"
        return
      fi
      ;;
  esac

  if ! command -v "$1" >/dev/null 2>&1; then
    log "$RED" "ERROR" "Missing command: $1"
    exit 1
  fi
}

start_backend() {
  if (( START_BACKEND == 0 )); then
    log "$YELLOW" "BACKEND" "Skipped by --no-backend"
    return
  fi

  if port_open "$BACKEND_HOST" "$BACKEND_PORT"; then
    log "$YELLOW" "BACKEND" "$BACKEND_HOST:$BACKEND_PORT is already listening; reusing it"
    return
  fi

  need_cmd go

  if [[ ! -f "$AUTHLIB_INJECTOR_PATH" ]]; then
    log "$RED" "ERROR" "authlib-injector jar not found: $AUTHLIB_INJECTOR_PATH"
    exit 1
  fi

  log "$GREEN" "BACKEND" "Starting backend at $SERVER_ADDR"
  (
    cd "$ROOT_DIR/backend"
    export SERVER_ADDR
    export PUBLIC_BASE_URL
    export AUTHLIB_INJECTOR_PATH
    export APP_ENV="${APP_ENV:-development}"
    export JWT_SECRET="${JWT_SECRET:-dev-local-launcher-secret}"
    export ALLOWED_ORIGINS="${ALLOWED_ORIGINS:-http://127.0.0.1:3000,http://localhost:3000,http://127.0.0.1:5173,http://localhost:5173}"
    exec go run ./cmd/server
  ) > >(prefix_output "$GREEN" "BACKEND") 2>&1 &
  track_pid "$!" "backend"

  wait_for_port "BACKEND" "$BACKEND_HOST" "$BACKEND_PORT" 30 "$GREEN"
}

start_mc_server() {
  if (( START_MC_SERVER == 0 )); then
    log "$YELLOW" "MC" "Skipped by --no-server"
    return
  fi

  if port_open "$MC_HOST" "$MC_PORT"; then
    log "$YELLOW" "MC" "$MC_HOST:$MC_PORT is already listening; reusing it"
    return
  fi

  need_cmd java

  if [[ ! -d "$SERVER_DIR" ]]; then
    log "$RED" "ERROR" "Minecraft server directory not found: $SERVER_DIR"
    exit 1
  fi
  if [[ ! -f "$SERVER_DIR/run.sh" ]]; then
    log "$RED" "ERROR" "run.sh not found in: $SERVER_DIR"
    exit 1
  fi

  log "$BLUE" "MC" "Starting Minecraft server from $SERVER_DIR"
  (
    cd "$SERVER_DIR"
    exec sh ./run.sh
  ) > >(prefix_output "$BLUE" "MC") 2>&1 &
  track_pid "$!" "minecraft-server"

  wait_for_port "MC" "$MC_HOST" "$MC_PORT" 60 "$BLUE"
}

start_launcher() {
  if (( START_LAUNCHER == 0 )); then
    log "$YELLOW" "LAUNCHER" "Skipped by --no-launcher"
    return
  fi

  local launcher_bin="$ROOT_DIR/launcher-slint/target/release/launcher-slint"

  if (( BUILD_LAUNCHER == 1 )) || [[ ! -x "$launcher_bin" ]]; then
    need_cmd cargo
    log "$GRAY" "LAUNCHER" "Building launcher release binary"
    (
      cd "$ROOT_DIR/launcher-slint"
      cargo build --release
    ) > >(prefix_output "$GRAY" "LAUNCHER") 2>&1
  fi

  if [[ ! -x "$launcher_bin" ]]; then
    log "$RED" "ERROR" "Launcher binary not found: $launcher_bin"
    exit 1
  fi

  log "$GRAY" "LAUNCHER" "Starting launcher with LAUNCHER_API_URL=$PUBLIC_BASE_URL"
  (
    cd "$ROOT_DIR"
    export LAUNCHER_API_URL="$PUBLIC_BASE_URL"
    exec "$launcher_bin"
  ) > >(prefix_output "$GRAY" "LAUNCHER") 2>&1 &
  track_pid "$!" "launcher"
}

echo ""
log "$GRAY" "SYSTEM" "Project Minecraft local test stack"
printf '%sBackend:%s   %s\n' "$DIM" "$RESET" "$PUBLIC_BASE_URL"
printf '%sMC server:%s %s:%s\n' "$DIM" "$RESET" "$MC_HOST" "$MC_PORT"
printf '%sServer dir:%s %s\n' "$DIM" "$RESET" "$SERVER_DIR"
printf '%sInjector:%s   %s\n' "$DIM" "$RESET" "$AUTHLIB_INJECTOR_PATH"
echo ""

start_backend
start_mc_server
start_launcher

echo ""
log "$GREEN" "SYSTEM" "Test stack is up"
log "$GRAY" "SYSTEM" "Connect in Minecraft to localhost:$MC_PORT"
log "$GRAY" "SYSTEM" "Press Ctrl+C here to stop services started by this script"
echo ""

wait
