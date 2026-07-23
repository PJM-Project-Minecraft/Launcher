#!/usr/bin/env bash
#
# Сборка плеер-лаунчера с зашитым URL бэкенда и (опционально) подписью автообновления.
#
# Подпись: задайте приватный ключ — публичный ВЫВОДИТСЯ из него автоматически, вшивается
# в бинарник и им же подписывается сборка. Ключ создаётся `updatesign keygen`, хранится
# ТОЛЬКО на релиз-боксе (в git/на сервере его нет).
#   scripts/prod/build-player-launcher.sh --api-url https://... --signing-key ~/pjm-update-signing.key
# Без ключа собирается лаунчер, принимающий обновления по SHA (как раньше).

set -euo pipefail

ROOT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
API_URL="${LAUNCHER_DEFAULT_API_URL:-}"
OUT_DIR="$ROOT_DIR/dist/releases"
BUILD=1
# Ключ подписи и публичный ключ можно задать флагом или переменной окружения.
SIGNING_KEY="${LAUNCHER_SIGNING_KEY:-}"
PUBKEY="${LAUNCHER_UPDATE_PUBKEY:-}"

usage() {
  cat <<'EOF'
Usage: scripts/prod/build-player-launcher.sh --api-url https://launcher.example.com [options]

Options:
  --api-url URL       Public backend URL used by players and the game server
  --signing-key PATH  Приватный Ed25519-ключ (updatesign keygen). Публичный ключ
                      выводится из него, вшивается в бинарник и им же подписывается сборка.
                      Можно вместо флага задать переменную LAUNCHER_SIGNING_KEY.
  --out-dir DIR       Output directory (default: dist/releases)
  --no-build          Package the existing release binary without rebuilding
  -h, --help          Show this help

Example (с подписью):
  scripts/prod/build-player-launcher.sh --api-url https://launcher.example.com \
    --signing-key ~/pjm-update-signing.key
EOF
}

while [[ $# -gt 0 ]]; do
  case "$1" in
    --api-url)
      API_URL="${2:-}"
      shift
      ;;
    --signing-key)
      SIGNING_KEY="${2:-}"
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

# run_updatesign — вызывает инструмент подписи: собранный бинарь updatesign в PATH
# или `go run` из модуля backend. Пути аргументов должны быть абсолютными (go run
# исполняется с cwd=backend).
run_updatesign() {
  if command -v updatesign >/dev/null 2>&1; then
    updatesign "$@"
  elif command -v go >/dev/null 2>&1; then
    ( cd "$ROOT_DIR/backend" && go run ./cmd/updatesign "$@" )
  else
    echo "ERROR: для подписи нужен updatesign в PATH или go (backend/cmd/updatesign)." >&2
    return 1
  fi
}

# Публичный ключ выводим из приватного (одна точка правды): пользователь задаёт только
# ключ, копировать pubkey руками не нужно — так исчезает ошибка «забыл экспортировать».
if [[ -n "$SIGNING_KEY" ]]; then
  if [[ ! -f "$SIGNING_KEY" ]]; then
    echo "ERROR: файл приватного ключа не найден: $SIGNING_KEY" >&2
    exit 1
  fi
  DERIVED_PUB="$(run_updatesign pubkey -key "$SIGNING_KEY")" || exit 1
  if [[ -n "$PUBKEY" && "$PUBKEY" != "$DERIVED_PUB" ]]; then
    echo "ERROR: LAUNCHER_UPDATE_PUBKEY не совпадает с ключом из --signing-key." >&2
    exit 1
  fi
  PUBKEY="$DERIVED_PUB"
  echo "[launcher] Публичный ключ выведен из приватного: $PUBKEY"
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
    # PUBKEY передаётся ЯВНО (не через export родителя) — иначе option_env! его не увидит.
    LAUNCHER_DEFAULT_API_URL="$API_URL" LAUNCHER_UPDATE_PUBKEY="$PUBKEY" cargo build --release
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

PLAYER_BIN="$PACKAGE_DIR/project-minecraft-launcher"
[[ "$PLATFORM" == windows-* ]] && PLAYER_BIN="$PACKAGE_DIR/ProjectMinecraftLauncher.exe"

# Подпись автообновления (Ed25519). Публичный ключ вшивается через option_env! на этапе
# cargo — проверяем, что он реально попал в бинарник (страховка от кэша cargo/сбоя).
if [[ -n "$PUBKEY" ]]; then
  if grep -aq "$PUBKEY" "$PLAYER_BIN"; then
    echo "[launcher] Публичный ключ вшит в бинарник ✓"
  else
    echo "[launcher] ОШИБКА: публичный ключ задан, но в бинарнике его НЕТ (cargo не пересобрал?)." >&2
    echo "[launcher]        Удали launcher-slint/target/release и пересобери." >&2
    exit 1
  fi
fi

if [[ -n "$SIGNING_KEY" ]]; then
  SIG="$(run_updatesign sign -key "$SIGNING_KEY" "$PLAYER_BIN")" || exit 1
  echo "$SIG" > "$PACKAGE_DIR/signature.txt"
  # Сразу проверяем подпись тем же публичным ключом — ровно как это сделает лаунчер.
  run_updatesign verify -pub "$PUBKEY" -sig "$SIG" "$PLAYER_BIN" || {
    echo "[launcher] ОШИБКА: собственная проверка подписи не прошла." >&2; exit 1; }
  echo "[launcher] Подпись обновления ($PLATFORM): $SIG"
  echo "[launcher] Сохранена в $PACKAGE_DIR/signature.txt — вставьте её в поле «Подпись» при заливке релиза."
else
  echo "[launcher] (без подписи: --signing-key не задан; лаунчер со вшитым ключом отвергнет неподписанный релиз)" >&2
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
