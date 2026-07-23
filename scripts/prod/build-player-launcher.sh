#!/usr/bin/env bash
#
# Build a player-facing launcher package with the production backend URL baked in.

set -euo pipefail

ROOT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
API_URL="${LAUNCHER_DEFAULT_API_URL:-}"
OUT_DIR="$ROOT_DIR/dist/releases"
BUILD=1

usage() {
  cat <<'EOF'
Usage: scripts/prod/build-player-launcher.sh --api-url https://launcher.example.com [options]

Options:
  --api-url URL      Public backend URL used by players and the game server
  --out-dir DIR      Output directory (default: dist/releases)
  --no-build         Package the existing release binary without rebuilding
  -h, --help         Show this help

Example:
  scripts/prod/build-player-launcher.sh --api-url https://launcher.example.com
EOF
}

while [[ $# -gt 0 ]]; do
  case "$1" in
    --api-url)
      API_URL="${2:-}"
      shift
      ;;
    --out-dir)
      OUT_DIR="${2:-}"
      shift
      ;;
    --no-build)
      BUILD=0
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

if [[ -z "$API_URL" ]]; then
  echo "ERROR: --api-url is required." >&2
  usage >&2
  exit 2
fi

if [[ "$API_URL" != http://* && "$API_URL" != https://* ]]; then
  echo "ERROR: --api-url must start with http:// or https://." >&2
  exit 2
fi

if ! command -v cargo >/dev/null 2>&1; then
  if [[ -x "$HOME/.cargo/bin/cargo" ]]; then
    export PATH="$HOME/.cargo/bin:$PATH"
  else
    echo "ERROR: cargo not found. Install Rust or source ~/.cargo/env." >&2
    exit 1
  fi
fi

VERSION="$(awk -F '"' '/^version = / { print $2; exit }' "$ROOT_DIR/launcher-slint/Cargo.toml")"
TARGET_TRIPLE="$(rustc -vV | sed -n 's/^host: //p')"
PLATFORM="linux-x64"
case "$TARGET_TRIPLE" in
  *aarch64*linux*) PLATFORM="linux-arm64" ;;
  *x86_64*linux*) PLATFORM="linux-x64" ;;
  *windows*) PLATFORM="windows-x64" ;;
  *darwin*) PLATFORM="macos" ;;
esac

if (( BUILD == 1 )); then
  echo "[launcher] Building release with LAUNCHER_DEFAULT_API_URL=$API_URL"
  (
    cd "$ROOT_DIR/launcher-slint"
    LAUNCHER_DEFAULT_API_URL="$API_URL" cargo build --release
  )
fi

BIN_NAME="launcher-slint"
[[ "$PLATFORM" == windows-* ]] && BIN_NAME="launcher-slint.exe"
SOURCE_BIN="$ROOT_DIR/launcher-slint/target/release/$BIN_NAME"

if [[ ! -x "$SOURCE_BIN" ]]; then
  echo "ERROR: release binary not found: $SOURCE_BIN" >&2
  exit 1
fi

PACKAGE_NAME="project-minecraft-launcher-${VERSION}-${PLATFORM}"
PACKAGE_DIR="$OUT_DIR/$PACKAGE_NAME"
rm -rf "$PACKAGE_DIR"
mkdir -p "$PACKAGE_DIR"

if [[ "$PLATFORM" == windows-* ]]; then
  cp "$SOURCE_BIN" "$PACKAGE_DIR/ProjectMinecraftLauncher.exe"
else
  cp "$SOURCE_BIN" "$PACKAGE_DIR/project-minecraft-launcher"
  cat > "$PACKAGE_DIR/run.sh" <<'EOF'
#!/usr/bin/env sh
DIR="$(CDPATH= cd -- "$(dirname -- "$0")" && pwd)"
exec "$DIR/project-minecraft-launcher" "$@"
EOF
  chmod +x "$PACKAGE_DIR/run.sh" "$PACKAGE_DIR/project-minecraft-launcher"
fi

cat > "$PACKAGE_DIR/README.txt" <<EOF
Project Minecraft Launcher

Backend:
  $API_URL

Linux:
  ./run.sh

Windows:
  ProjectMinecraftLauncher.exe

Do not set LAUNCHER_API_URL unless you intentionally want to override the
production backend.
EOF

# Оффлайн-подпись бинарника обновления (Ed25519). Приватный ключ — ТОЛЬКО на релиз-боксе
# (в git/на сервере его нет). Задайте LAUNCHER_SIGNING_KEY=путь/к/update-signing.key;
# ключ создаётся `updatesign keygen`, публичный (LAUNCHER_UPDATE_PUBKEY) вшивается в сборку.
# Лаунчер со вшитым ключом примет обновление ТОЛЬКО с валидной подписью.
PLAYER_BIN="$PACKAGE_DIR/project-minecraft-launcher"
[[ "$PLATFORM" == windows-* ]] && PLAYER_BIN="$PACKAGE_DIR/ProjectMinecraftLauncher.exe"

# Публичный ключ вшивается через option_env! на этапе cargo. Частая ошибка — задать
# LAUNCHER_UPDATE_PUBKEY отдельной (не экспортированной) строкой: тогда он не долетает
# до cargo и лаунчер молча собирается БЕЗ проверки подписи. Ловим это сразу.
if [[ -n "${LAUNCHER_UPDATE_PUBKEY:-}" ]]; then
  if grep -aq "$LAUNCHER_UPDATE_PUBKEY" "$PLAYER_BIN"; then
    echo "[launcher] Публичный ключ вшит в бинарник ✓"
  else
    echo "[launcher] ОШИБКА: LAUNCHER_UPDATE_PUBKEY задан, но в бинарнике его НЕТ." >&2
    echo "[launcher]        Переменная не экспортирована в cargo. Передавай её В ОДНОЙ строке" >&2
    echo "[launcher]        со скриптом (LAUNCHER_UPDATE_PUBKEY=... scripts/prod/build-...) или через export." >&2
    exit 1
  fi
elif [[ -n "${LAUNCHER_SIGNING_KEY:-}" ]]; then
  echo "[launcher] ВНИМАНИЕ: подписываешь релиз, но LAUNCHER_UPDATE_PUBKEY не задан —" >&2
  echo "[launcher]          собранный лаунчер НЕ проверяет подпись. Забыл экспортировать ключ?" >&2
fi

if [[ -n "${LAUNCHER_SIGNING_KEY:-}" ]]; then
  SIG=""
  if command -v updatesign >/dev/null 2>&1; then
    SIG="$(updatesign sign -key "$LAUNCHER_SIGNING_KEY" "$PLAYER_BIN")"
  elif command -v go >/dev/null 2>&1; then
    SIG="$(cd "$ROOT_DIR/backend" && go run ./cmd/updatesign sign -key "$LAUNCHER_SIGNING_KEY" "$PLAYER_BIN")"
  else
    echo "[launcher] WARN: LAUNCHER_SIGNING_KEY задан, но нет ни updatesign в PATH, ни go — подпись пропущена." >&2
  fi
  if [[ -n "$SIG" ]]; then
    echo "$SIG" > "$PACKAGE_DIR/signature.txt"
    echo "[launcher] Подпись обновления ($PLATFORM): $SIG"
    echo "[launcher] Сохранена в $PACKAGE_DIR/signature.txt — вставьте её в поле «Подпись» при заливке релиза."
  fi
else
  echo "[launcher] (подпись не создана: LAUNCHER_SIGNING_KEY не задан; лаунчер со вшитым ключом отвергнет неподписанный релиз)" >&2
fi

mkdir -p "$OUT_DIR"
if command -v tar >/dev/null 2>&1; then
  (
    cd "$OUT_DIR"
    tar -czf "$PACKAGE_NAME.tar.gz" "$PACKAGE_NAME"
  )
  echo "[launcher] Package: $OUT_DIR/$PACKAGE_NAME.tar.gz"
else
  echo "[launcher] Package directory: $PACKAGE_DIR"
fi
