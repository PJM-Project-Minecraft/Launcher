#!/usr/bin/env bash
# Offline gate for the two immutable launcher artifacts. Writes SHA256SUMS only
# after both signatures, embedded keys, version markers and PE imports pass.

set -euo pipefail

ROOT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
ARTIFACT_DIR=""
VERSION=""
UPDATE_PUBKEY=""
MANIFEST_PUBKEY=""

usage() {
  cat <<'EOF'
Usage: scripts/prod/verify-player-launcher-artifacts.sh \
  --dir DIR --version X.Y.Z --update-pubkey HEX --manifest-pubkey HEX
EOF
}

while [[ $# -gt 0 ]]; do
  case "$1" in
    --dir) ARTIFACT_DIR="${2:-}"; shift ;;
    --version) VERSION="${2:-}"; shift ;;
    --update-pubkey) UPDATE_PUBKEY="${2:-}"; shift ;;
    --manifest-pubkey) MANIFEST_PUBKEY="${2:-}"; shift ;;
    -h|--help) usage; exit 0 ;;
    *) echo "ERROR: unknown option: $1" >&2; usage >&2; exit 2 ;;
  esac
  shift
done

[[ -d "$ARTIFACT_DIR" ]] || { echo "ERROR: artifact directory is missing" >&2; exit 2; }
[[ "$VERSION" =~ ^[0-9]+\.[0-9]+\.[0-9]+$ ]] || { echo "ERROR: invalid version" >&2; exit 2; }
[[ "$UPDATE_PUBKEY" =~ ^[0-9a-f]{64}$ ]] || { echo "ERROR: invalid update public key" >&2; exit 2; }
[[ "$MANIFEST_PUBKEY" =~ ^[0-9a-f]{64}$ ]] || { echo "ERROR: invalid manifest public key" >&2; exit 2; }

run_updatesign() {
  if command -v updatesign >/dev/null 2>&1; then
    updatesign "$@"
  elif command -v go >/dev/null 2>&1; then
    (cd "$ROOT_DIR/backend" && go run ./cmd/updatesign "$@")
  else
    echo "ERROR: updatesign or go is required" >&2
    return 1
  fi
}

linux_dir="$ARTIFACT_DIR/project-minecraft-launcher-$VERSION-linux-x64"
windows_dir="$ARTIFACT_DIR/project-minecraft-launcher-$VERSION-windows-x64"
linux_bin="$linux_dir/project-minecraft-launcher"
windows_bin="$windows_dir/ProjectMinecraftLauncher.exe"
linux_archive="$ARTIFACT_DIR/project-minecraft-launcher-$VERSION-linux-x64.tar.gz"
windows_archive="$ARTIFACT_DIR/project-minecraft-launcher-$VERSION-windows-x64.tar.gz"

for path in "$linux_bin" "$windows_bin" "$linux_archive" "$windows_archive" \
  "$linux_dir/signature.txt" "$windows_dir/signature.txt"; do
  [[ -s "$path" ]] || { echo "ERROR: missing artifact: $path" >&2; exit 1; }
done
[[ -x "$linux_bin" ]] || { echo "ERROR: Linux artifact is not executable" >&2; exit 1; }

for binary in "$linux_bin" "$windows_bin"; do
  grep -aq "PMLVER=$VERSION;" "$binary" || { echo "ERROR: version marker missing in $binary" >&2; exit 1; }
  grep -aq "$UPDATE_PUBKEY" "$binary" || { echo "ERROR: update key missing in $binary" >&2; exit 1; }
  grep -aq "$MANIFEST_PUBKEY" "$binary" || { echo "ERROR: manifest key missing in $binary" >&2; exit 1; }
done

run_updatesign verify -pub "$UPDATE_PUBKEY" -sig "$(tr -d '\r\n' <"$linux_dir/signature.txt")" "$linux_bin"
run_updatesign verify -pub "$UPDATE_PUBKEY" -sig "$(tr -d '\r\n' <"$windows_dir/signature.txt")" "$windows_bin"

objdump_bin=""
if command -v x86_64-w64-mingw32-objdump >/dev/null 2>&1; then
  objdump_bin="x86_64-w64-mingw32-objdump"
elif command -v objdump >/dev/null 2>&1; then
  objdump_bin="objdump"
fi
[[ -n "$objdump_bin" ]] || { echo "ERROR: objdump is required" >&2; exit 1; }
imports="$($objdump_bin -p "$windows_bin")"
if grep -Eiq 'DLL Name: (WebView2Loader|VCRUNTIME|MSVCP)[^ ]*\.dll' <<<"$imports"; then
  echo "ERROR: Windows artifact imports a forbidden external runtime DLL" >&2
  exit 1
fi

tar -tzf "$linux_archive" >/dev/null
tar -tzf "$windows_archive" >/dev/null
verify_dir="$(mktemp -d)"
trap 'rm -rf -- "$verify_dir"' EXIT
tar -xzf "$linux_archive" -C "$verify_dir"
tar -xzf "$windows_archive" -C "$verify_dir"
cmp -s "$linux_bin" "$verify_dir/project-minecraft-launcher-$VERSION-linux-x64/project-minecraft-launcher" || {
  echo "ERROR: Linux archive does not contain the verified binary" >&2
  exit 1
}
cmp -s "$windows_bin" "$verify_dir/project-minecraft-launcher-$VERSION-windows-x64/ProjectMinecraftLauncher.exe" || {
  echo "ERROR: Windows archive does not contain the verified binary" >&2
  exit 1
}
cmp -s "$linux_dir/signature.txt" "$verify_dir/project-minecraft-launcher-$VERSION-linux-x64/signature.txt" || {
  echo "ERROR: Linux archive does not contain the verified signature" >&2
  exit 1
}
cmp -s "$windows_dir/signature.txt" "$verify_dir/project-minecraft-launcher-$VERSION-windows-x64/signature.txt" || {
  echo "ERROR: Windows archive does not contain the verified signature" >&2
  exit 1
}
(
  cd "$ARTIFACT_DIR"
  sha256sum \
    "$(basename "$linux_archive")" \
    "$(basename "$windows_archive")" \
    > SHA256SUMS
  sha256sum \
    "project-minecraft-launcher-$VERSION-linux-x64/project-minecraft-launcher" \
    "project-minecraft-launcher-$VERSION-linux-x64/signature.txt" \
    "project-minecraft-launcher-$VERSION-windows-x64/ProjectMinecraftLauncher.exe" \
    "project-minecraft-launcher-$VERSION-windows-x64/signature.txt" \
    >> SHA256SUMS
)

echo "OK: Linux/Windows $VERSION artifacts verified; checksums: $ARTIFACT_DIR/SHA256SUMS"
