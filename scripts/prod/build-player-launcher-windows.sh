#!/usr/bin/env bash
# Reproducible self-contained Windows MSVC build through cargo-xwin + LLVM 18.

set -euo pipefail

ROOT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
IMAGE="launcher-windows-xwin:rust-1.96-cargo-xwin-0.23.1"
API_URL="${LAUNCHER_DEFAULT_API_URL:-}"
SIGNING_KEY="${LAUNCHER_SIGNING_KEY:-}"
MANIFEST_PUBKEY="${DELIVERY_MANIFEST_PUBKEY:-}"
OUT_DIR="release-artifacts"
NO_BUILD=0

usage() {
  cat <<'EOF'
Usage: scripts/prod/build-player-launcher-windows.sh --api-url URL \
  --signing-key PATH --manifest-pubkey HEX [--out-dir RELATIVE_DIR] [--no-build]

The output directory must be inside the repository. No artifact is uploaded.
EOF
}

while [[ $# -gt 0 ]]; do
  case "$1" in
    --api-url) API_URL="${2:-}"; shift ;;
    --signing-key) SIGNING_KEY="${2:-}"; shift ;;
    --manifest-pubkey) MANIFEST_PUBKEY="${2:-}"; shift ;;
    --out-dir) OUT_DIR="${2:-}"; shift ;;
    --no-build) NO_BUILD=1 ;;
    -h|--help) usage; exit 0 ;;
    *) echo "ERROR: unknown option: $1" >&2; usage >&2; exit 2 ;;
  esac
  shift
done

[[ "$API_URL" == https://* ]] || { echo "ERROR: production API URL must use https://" >&2; exit 2; }
[[ -f "$SIGNING_KEY" ]] || { echo "ERROR: signing key is missing" >&2; exit 2; }
[[ "$MANIFEST_PUBKEY" =~ ^[0-9a-f]{64}$ ]] || { echo "ERROR: manifest public key must be 64 lowercase hex" >&2; exit 2; }
[[ "$OUT_DIR" != /* && "$OUT_DIR" != *".."* ]] || { echo "ERROR: --out-dir must be a safe repository-relative path" >&2; exit 2; }

CACHE_DIR="${XDG_CACHE_HOME:-$HOME/.cache}/project-minecraft-launcher/xwin-0.23.1"
mkdir -p "$CACHE_DIR/home" "$CACHE_DIR/cargo"

docker build -q -f "$ROOT_DIR/scripts/prod/windows-xwin.Dockerfile" -t "$IMAGE" "$ROOT_DIR" >/dev/null
build_args=()
if (( NO_BUILD == 1 )); then
  build_args+=(--no-build)
fi
docker run --rm \
  --user "$(id -u):$(id -g)" \
  -e HOME=/cache/home \
  -e CARGO_HOME=/cache/cargo \
  -e RUSTUP_HOME=/opt/rustup \
  -e PATH=/cache/cargo/bin:/opt/cargo/bin:/usr/lib/llvm-18/bin:/usr/local/bin:/usr/bin:/bin \
  -v "$ROOT_DIR:/work" \
  -v "$CACHE_DIR:/cache" \
  -v "$SIGNING_KEY:/run/secrets/update-signing.key:ro" \
  -w /work \
  "$IMAGE" \
  scripts/prod/build-player-launcher.sh \
    --api-url "$API_URL" \
    --signing-key /run/secrets/update-signing.key \
    --manifest-pubkey "$MANIFEST_PUBKEY" \
    --target x86_64-pc-windows-msvc \
    --out-dir "/work/$OUT_DIR" \
    "${build_args[@]}"
