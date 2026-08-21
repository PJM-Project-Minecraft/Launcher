#!/usr/bin/env bash
#
# Сборка плеер-лаунчера с зашитым URL бэкенда и (опционально) подписью автообновления.
#
# Подпись: задайте приватный ключ — публичный ВЫВОДИТСЯ из него автоматически, вшивается
# в бинарник и им же подписывается сборка. Ключ создаётся `updatesign keygen`, хранится
# ТОЛЬКО на релиз-боксе (в git/на сервере его нет).
#   scripts/prod/build-player-launcher.sh --api-url https://... --signing-key ~/pjm-update-signing.key
# Release-сборка без обоих публичных ключей запрещена.

set -euo pipefail

ROOT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
API_URL="${LAUNCHER_DEFAULT_API_URL:-}"
OUT_DIR="$ROOT_DIR/release-artifacts"
BUILD=1
BUILD_ONLY=0
TARGET_TRIPLE=""
# Ключ подписи и публичный ключ можно задать флагом или переменной окружения.
SIGNING_KEY="${LAUNCHER_SIGNING_KEY:-}"
PUBKEY="${LAUNCHER_UPDATE_PUBKEY:-}"
MANIFEST_PUBKEY="${DELIVERY_MANIFEST_PUBKEY:-}"

usage() {
  cat <<'EOF'
Usage: scripts/prod/build-player-launcher.sh --api-url https://launcher.example.com [options]

Options:
  --api-url URL       Public backend URL used by players and the game server
  --signing-key PATH  Приватный Ed25519-ключ (updatesign keygen). Публичный ключ
                      выводится из него, вшивается в бинарник и им же подписывается сборка.
                      Можно вместо флага задать переменную LAUNCHER_SIGNING_KEY.
  --manifest-pubkey HEX  Публичный Ed25519-ключ подписи delivery manifest.
                         Можно задать DELIVERY_MANIFEST_PUBKEY.
  --out-dir DIR       Output directory (default: release-artifacts, outside Vite dist)
  --target TRIPLE     Rust target; Windows MSVC automatically uses cargo-xwin
  --build-only        Build and verify the binary without packaging or signing
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
    --manifest-pubkey)
      MANIFEST_PUBKEY="${2:-}"
      shift
      ;;
    --out-dir)
      OUT_DIR="${2:-}"
      shift
      ;;
    --target)
      TARGET_TRIPLE="${2:-}"
      shift
      ;;
    --no-build)
      BUILD=0
      ;;
    --build-only)
      BUILD_ONLY=1
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

if [[ ! "$MANIFEST_PUBKEY" =~ ^[0-9a-f]{64}$ ]]; then
  echo "ERROR: --manifest-pubkey / DELIVERY_MANIFEST_PUBKEY должен содержать 64 lowercase hex-символа." >&2
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

if [[ ! "$PUBKEY" =~ ^[0-9a-f]{64}$ ]]; then
  echo "ERROR: задайте --signing-key либо LAUNCHER_UPDATE_PUBKEY (64 lowercase hex-символа)." >&2
  exit 2
fi

VERSION="$(awk -F '"' '/^version = / { print $2; exit }' "$ROOT_DIR/src-tauri/Cargo.toml")"
SOURCE_DATE_EPOCH="${SOURCE_DATE_EPOCH:-$(git -C "$ROOT_DIR" log -1 --format=%ct)}"
SOURCE_COMMIT="${SOURCE_COMMIT:-$(git -C "$ROOT_DIR" rev-parse HEAD 2>/dev/null || true)}"
[[ "$SOURCE_DATE_EPOCH" =~ ^[0-9]+$ ]] || {
  echo "[launcher] ОШИБКА: SOURCE_DATE_EPOCH должен быть Unix timestamp." >&2
  exit 1
}
[[ "$SOURCE_COMMIT" =~ ^[0-9a-f]{40}$ ]] || {
  echo "[launcher] ОШИБКА: SOURCE_COMMIT должен быть полным SHA-1 commit." >&2
  exit 1
}
if (( BUILD_ONLY == 1 && BUILD == 0 )); then
  echo "[launcher] ОШИБКА: --build-only и --no-build несовместимы." >&2
  exit 2
fi
export SOURCE_DATE_EPOCH TZ=UTC LC_ALL=C
if [[ -z "$TARGET_TRIPLE" ]]; then
  TARGET_TRIPLE="$(rustc -vV | sed -n 's/^host: //p')"
fi
PLATFORM="linux-x64"
case "$TARGET_TRIPLE" in
  *aarch64*linux*) PLATFORM="linux-arm64" ;;
  *x86_64*linux*) PLATFORM="linux-x64" ;;
  *windows*) PLATFORM="windows-x64" ;;
  *darwin*) PLATFORM="macos" ;;
esac

if (( BUILD == 1 )); then
  echo "[launcher] Building release with LAUNCHER_DEFAULT_API_URL=$API_URL"
  # Важно собирать через Tauri CLI, а не через голый `cargo build`: CLI переключает
  # WebView с devUrl (Vite на 127.0.0.1:1420) на встроенный frontendDist. Иначе Rust
  # бинарник формально release, но у игроков открывает недоступный dev-сервер.
  build_args=(build --no-bundle --target "$TARGET_TRIPLE")
  if [[ "$TARGET_TRIPLE" == *windows-msvc ]]; then
    if ! command -v cargo-xwin >/dev/null 2>&1 && ! cargo xwin --version >/dev/null 2>&1; then
      echo "ERROR: Windows MSVC build requires cargo-xwin." >&2
      exit 1
    fi
    build_args=(build --no-bundle --runner cargo-xwin --target "$TARGET_TRIPLE")
    export RUSTFLAGS="${RUSTFLAGS:+$RUSTFLAGS }-C link-arg=/Brepro"
  fi
  (
    cd "$ROOT_DIR"
    npm ci
    LAUNCHER_DEFAULT_API_URL="$API_URL" \
      LAUNCHER_UPDATE_PUBKEY="$PUBKEY" \
      DELIVERY_MANIFEST_PUBKEY="$MANIFEST_PUBKEY" \
      npm run tauri -- "${build_args[@]}"
  )
fi

BIN_NAME="project-minecraft-launcher"
[[ "$PLATFORM" == windows-* ]] && BIN_NAME="project-minecraft-launcher.exe"
SOURCE_BIN="$ROOT_DIR/src-tauri/target/$TARGET_TRIPLE/release/$BIN_NAME"
if [[ "$TARGET_TRIPLE" == "$(rustc -vV | sed -n 's/^host: //p')" && ! -f "$SOURCE_BIN" ]]; then
  SOURCE_BIN="$ROOT_DIR/src-tauri/target/release/$BIN_NAME"
fi

if [[ ! -f "$SOURCE_BIN" ]] || [[ "$PLATFORM" != windows-* && ! -x "$SOURCE_BIN" ]]; then
  echo "ERROR: release binary not found: $SOURCE_BIN" >&2
  exit 1
fi

# Release binary validation runs before packaging, so the networked build phase
# can stop here without ever receiving the private signing key.
if [[ "$PLATFORM" == windows-* ]]; then
  OBJDUMP=""
  if command -v x86_64-w64-mingw32-objdump >/dev/null 2>&1; then
    OBJDUMP="x86_64-w64-mingw32-objdump"
  elif command -v objdump >/dev/null 2>&1; then
    OBJDUMP="objdump"
  else
    echo "[launcher] ОШИБКА: objdump обязателен для проверки Windows imports." >&2
    exit 1
  fi
  IMPORTS="$($OBJDUMP -p "$SOURCE_BIN")"
  if grep -Eiq 'DLL Name: (WebView2Loader|VCRUNTIME|MSVCP)[^ ]*\.dll' <<<"$IMPORTS"; then
    echo "[launcher] ОШИБКА: Windows-бинарник зависит от внешней DLL:" >&2
    grep -Ei 'DLL Name: (WebView2Loader|VCRUNTIME|MSVCP)[^ ]*\.dll' <<<"$IMPORTS" >&2
    exit 1
  fi
fi

grep -aq "$PUBKEY" "$SOURCE_BIN" || {
  echo "[launcher] ОШИБКА: публичный update-ключ не вшит в бинарник." >&2
  exit 1
}
grep -aq "$MANIFEST_PUBKEY" "$SOURCE_BIN" || {
  echo "[launcher] ОШИБКА: ключ delivery manifest не вшит в бинарник." >&2
  exit 1
}
grep -aq "$API_URL" "$SOURCE_BIN" || {
  echo "[launcher] ОШИБКА: production API URL не вшит в бинарник." >&2
  exit 1
}
grep -aq "PMLVER=$VERSION;" "$SOURCE_BIN" || {
  echo "[launcher] ОШИБКА: marker версии $VERSION не вшит в бинарник." >&2
  exit 1
}
echo "[launcher] Публичный ключ вшит в бинарник ✓"
echo "[launcher] Ключ delivery manifest вшит в бинарник ✓"

if (( BUILD_ONLY == 1 )); then
  echo "[launcher] Build-only phase complete; private signing key was not mounted."
  exit 0
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

cat > "$PACKAGE_DIR/BUILD-INFO.txt" <<EOF
source_commit=$SOURCE_COMMIT
source_date_epoch=$SOURCE_DATE_EPOCH
platform=$PLATFORM
api_url=$API_URL
update_pubkey=$PUBKEY
manifest_pubkey=$MANIFEST_PUBKEY
EOF

PLAYER_BIN="$PACKAGE_DIR/project-minecraft-launcher"
[[ "$PLATFORM" == windows-* ]] && PLAYER_BIN="$PACKAGE_DIR/ProjectMinecraftLauncher.exe"

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
    tar --sort=name --mtime="@$SOURCE_DATE_EPOCH" --owner=0 --group=0 --numeric-owner \
      -cf - "$PACKAGE_NAME" | gzip -n > "$PACKAGE_NAME.tar.gz"
  )
  echo "[launcher] Package: $OUT_DIR/$PACKAGE_NAME.tar.gz"
else
  echo "[launcher] Package directory: $PACKAGE_DIR"
fi
