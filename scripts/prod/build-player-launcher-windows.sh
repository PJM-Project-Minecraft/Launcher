#!/usr/bin/env bash
# Reproducible self-contained Windows MSVC build through cargo-xwin + LLVM 18.

set -euo pipefail

ROOT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
IMAGE="launcher-windows-xwin:rust-1.96-cargo-xwin-0.23.1"
API_URL="${LAUNCHER_DEFAULT_API_URL:-}"
SIGNING_KEY="${LAUNCHER_SIGNING_KEY:-}"
MANIFEST_PUBKEY="${DELIVERY_MANIFEST_PUBKEY:-}"
OUT_DIR="release-artifacts"

usage() {
  cat <<'EOF'
Usage: scripts/prod/build-player-launcher-windows.sh --api-url URL \
  --signing-key PATH --manifest-pubkey HEX [--out-dir RELATIVE_DIR]

The output directory must be inside the repository. No artifact is uploaded.
EOF
}

while [[ $# -gt 0 ]]; do
  case "$1" in
    --api-url) API_URL="${2:-}"; shift ;;
    --signing-key) SIGNING_KEY="${2:-}"; shift ;;
    --manifest-pubkey) MANIFEST_PUBKEY="${2:-}"; shift ;;
    --out-dir) OUT_DIR="${2:-}"; shift ;;
    -h|--help) usage; exit 0 ;;
    *) echo "ERROR: unknown option: $1" >&2; usage >&2; exit 2 ;;
  esac
  shift
done

[[ "$API_URL" == https://* ]] || { echo "ERROR: production API URL must use https://" >&2; exit 2; }
[[ -f "$SIGNING_KEY" ]] || { echo "ERROR: signing key is missing" >&2; exit 2; }
SIGNING_KEY="$(realpath "$SIGNING_KEY")"
[[ "$MANIFEST_PUBKEY" =~ ^[0-9a-f]{64}$ ]] || { echo "ERROR: manifest public key must be 64 lowercase hex" >&2; exit 2; }
[[ "$OUT_DIR" != /* && "$OUT_DIR" != *".."* ]] || { echo "ERROR: --out-dir must be a safe repository-relative path" >&2; exit 2; }

CACHE_DIR="${XDG_CACHE_HOME:-$HOME/.cache}/project-minecraft-launcher/xwin-0.23.1"
SOURCE_COMMIT="$(git -C "$ROOT_DIR" rev-parse HEAD)"
SOURCE_DATE_EPOCH="$(git -C "$ROOT_DIR" log -1 --format=%ct)"
[[ "$SOURCE_COMMIT" =~ ^[0-9a-f]{40}$ ]] || { echo "ERROR: cannot resolve source commit" >&2; exit 1; }
[[ "$SOURCE_DATE_EPOCH" =~ ^[0-9]+$ ]] || { echo "ERROR: cannot resolve commit timestamp" >&2; exit 1; }
[[ -z "$(git -C "$ROOT_DIR" status --porcelain --untracked-files=normal)" ]] || {
  echo "ERROR: production artifacts require a clean tracked worktree" >&2
  exit 1
}
mkdir -p "$CACHE_DIR/home/.npm/_cacache" "$CACHE_DIR/home/.cache/cargo-xwin" "$CACHE_DIR/cargo/registry" "$CACHE_DIR/cargo/git"
mkdir -p "$ROOT_DIR/node_modules" "$ROOT_DIR/dist" "$ROOT_DIR/src-tauri/target"
RELEASE_TEMP="$(mktemp -d)"
trap 'rm -rf -- "$RELEASE_TEMP"' EXIT

docker build -q -f "$ROOT_DIR/scripts/prod/windows-xwin.Dockerfile" -t "$IMAGE" "$ROOT_DIR" >/dev/null
UPDATE_PUBKEY="$(docker run --rm --network none --read-only \
  --user "$(id -u):$(id -g)" \
  --entrypoint /usr/local/bin/updatesign \
  -v "$SIGNING_KEY:/run/secrets/update-signing.key:ro" \
  "$IMAGE" pubkey -key /run/secrets/update-signing.key)"
[[ "$UPDATE_PUBKEY" =~ ^[0-9a-f]{64}$ ]] || { echo "ERROR: cannot derive update public key" >&2; exit 1; }

# Networked dependency/build phase receives only public material.
docker run --rm \
  --user "$(id -u):$(id -g)" \
  --tmpfs "/tmp/release-home:rw,uid=$(id -u),gid=$(id -g),mode=0700" \
  --tmpfs "/tmp/cargo-home:rw,uid=$(id -u),gid=$(id -g),mode=0700" \
  -e HOME=/tmp/release-home \
  -e CARGO_HOME=/tmp/cargo-home \
  -e RUSTUP_HOME=/opt/rustup \
  -e SOURCE_DATE_EPOCH="$SOURCE_DATE_EPOCH" \
  -e SOURCE_COMMIT="$SOURCE_COMMIT" \
  -e LAUNCHER_UPDATE_PUBKEY="$UPDATE_PUBKEY" \
  -e PATH=/opt/cargo/bin:/usr/lib/llvm-18/bin:/usr/local/bin:/usr/bin:/bin \
  -v "$ROOT_DIR:/work:ro" \
  -v "$ROOT_DIR/node_modules:/work/node_modules" \
  -v "$ROOT_DIR/dist:/work/dist" \
  -v "$ROOT_DIR/src-tauri/target:/work/src-tauri/target" \
  -v "$CACHE_DIR/home/.npm/_cacache:/tmp/release-home/.npm/_cacache" \
  -v "$CACHE_DIR/home/.cache/cargo-xwin:/tmp/release-home/.cache/cargo-xwin" \
  -v "$CACHE_DIR/cargo/registry:/tmp/cargo-home/registry" \
  -v "$CACHE_DIR/cargo/git:/tmp/cargo-home/git" \
  -w /work \
  "$IMAGE" \
  scripts/prod/build-player-launcher.sh \
    --api-url "$API_URL" \
    --manifest-pubkey "$MANIFEST_PUBKEY" \
    --target x86_64-pc-windows-msvc \
    --out-dir "/work/$OUT_DIR" \
    --build-only

# Sign only the fixed read-only binary with immutable tooling. The private key
# never shares a mount with the repository, build output directory or cache.
SOURCE_BIN="$ROOT_DIR/src-tauri/target/x86_64-pc-windows-msvc/release/project-minecraft-launcher.exe"
[[ -f "$SOURCE_BIN" ]] || { echo "ERROR: Windows release binary is missing" >&2; exit 1; }
BINARY_SHA_BEFORE="$(sha256sum "$SOURCE_BIN" | awk '{print $1}')"
SIGNATURE_DIR="$RELEASE_TEMP/signature"
mkdir -p "$SIGNATURE_DIR"
docker run --rm --network none --read-only \
  --user "$(id -u):$(id -g)" \
  --entrypoint /usr/local/bin/updatesign \
  -v "$SIGNING_KEY:/run/secrets/update-signing.key:ro" \
  -v "$SOURCE_BIN:/artifact:ro" \
  "$IMAGE" sign -key /run/secrets/update-signing.key /artifact \
  >"$SIGNATURE_DIR/signature.txt"
[[ "$BINARY_SHA_BEFORE" == "$(sha256sum "$SOURCE_BIN" | awk '{print $1}')" ]] || {
  echo "ERROR: binary changed during isolated signing" >&2
  exit 1
}

# Packaging receives the public signature, never the private key or build cache.
docker run --rm --network none \
  --user "$(id -u):$(id -g)" \
  -e HOME=/tmp \
  -e CARGO_HOME=/opt/cargo \
  -e RUSTUP_HOME=/opt/rustup \
  -e SOURCE_DATE_EPOCH="$SOURCE_DATE_EPOCH" \
  -e SOURCE_COMMIT="$SOURCE_COMMIT" \
  -e LAUNCHER_UPDATE_PUBKEY="$UPDATE_PUBKEY" \
  -e PATH=/opt/cargo/bin:/usr/lib/llvm-18/bin:/usr/local/bin:/usr/bin:/bin \
  -v "$ROOT_DIR:/work" \
  -v "$SIGNATURE_DIR/signature.txt:/run/signature.txt:ro" \
  -w /work \
  "$IMAGE" \
  scripts/prod/build-player-launcher.sh \
    --api-url "$API_URL" \
    --signature-file /run/signature.txt \
    --manifest-pubkey "$MANIFEST_PUBKEY" \
    --target x86_64-pc-windows-msvc \
    --out-dir "/work/$OUT_DIR" \
    --no-build
