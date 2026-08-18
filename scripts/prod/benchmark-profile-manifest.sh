#!/usr/bin/env bash
set -euo pipefail

host="${1:-root@176.108.254.89}"
base_url="${LAUNCHER_API_URL:-https://launcher.likonchik.xyz}"
runs="${MANIFEST_BENCH_RUNS:-3}"

mapfile -t credentials < <(
  ssh "$host" 'bash -s' <<'REMOTE'
set -euo pipefail
trap 'echo "remote benchmark setup failed at line $LINENO" >&2' ERR
cd /root/Launcher

user_id=$(docker compose exec -T postgres sh -c \
  'psql -U "$POSTGRES_USER" -d "$POSTGRES_DB" -Atc "SELECT id FROM users WHERE is_banned = false AND is_hwid_banned = false ORDER BY created_at LIMIT 1"' </dev/null)
profile_id=$(docker compose exec -T postgres sh -c \
  'psql -U "$POSTGRES_USER" -d "$POSTGRES_DB" -Atc "SELECT id FROM profiles WHERE is_active = true ORDER BY created_at LIMIT 1"' </dev/null)
jwt_secret=$(docker compose exec -T server printenv JWT_SECRET </dev/null)

TEST_USER_ID="$user_id" TEST_JWT_SECRET="$jwt_secret" python3 - <<'PY'
import base64
import hashlib
import hmac
import json
import os
import time

def encode(value):
    raw = json.dumps(value, separators=(",", ":")).encode()
    return base64.urlsafe_b64encode(raw).rstrip(b"=").decode()

now = int(time.time())
header = encode({"alg": "HS256", "typ": "JWT"})
payload = encode({"sub": os.environ["TEST_USER_ID"], "iat": now, "exp": now + 180})
body = f"{header}.{payload}"
signature = hmac.new(
    os.environ["TEST_JWT_SECRET"].encode(), body.encode(), hashlib.sha256
).digest()
print(body + "." + base64.urlsafe_b64encode(signature).rstrip(b"=").decode())
PY
printf '%s\n' "$profile_id"
REMOTE
)

if [[ ${#credentials[@]} -ne 2 ]]; then
  echo "failed to obtain temporary benchmark credentials (lines=${#credentials[@]})" >&2
  for credential in "${credentials[@]}"; do
    printf 'received_line_length=%s\n' "${#credential}" >&2
  done
  exit 1
fi

token="${credentials[0]}"
profile_id="${credentials[1]}"
url="${base_url%/}/api/profiles/${profile_id}/manifest"
body_file=$(mktemp)
header_file=$(mktemp)
trap 'rm -f "$body_file" "$header_file"' EXIT

for ((run = 1; run <= runs; run++)); do
  curl --silent --show-error --compressed \
    --header "Authorization: Bearer $token" \
    --dump-header "$header_file" \
    --output "$body_file" \
    --write-out "public run=$run code=%{http_code} wire_bytes=%{size_download} ttfb=%{time_starttransfer}s total=%{time_total}s\n" \
    "$url"
done

printf 'decoded_bytes=%s files=' "$(wc -c < "$body_file")"
python3 - "$body_file" <<'PY'
import json
import sys

with open(sys.argv[1], "rb") as manifest:
    print(len(json.load(manifest)["files"]))
PY

printf 'estimated_gzip_bytes=%s\n' "$(gzip -1 -c "$body_file" | wc -c)"
if command -v zstd >/dev/null 2>&1; then
  printf 'estimated_zstd_bytes=%s\n' "$(zstd -1 --quiet --stdout "$body_file" | wc -c)"
fi

encoding=$(awk 'BEGIN { IGNORECASE=1 } /^content-encoding:/ { gsub("\r", ""); print $2 }' "$header_file")
printf 'content_encoding=%s\n' "${encoding:-identity}"

printf '%s\n%s\n' "$token" "$profile_id" | ssh "$host" '
read -r token
read -r profile_id
curl --silent --show-error --compressed \
  --header "Authorization: Bearer $token" \
  --output /dev/null \
  --write-out "origin code=%{http_code} wire_bytes=%{size_download} ttfb=%{time_starttransfer}s total=%{time_total}s\\n" \
  "http://127.0.0.1:8080/api/profiles/$profile_id/manifest"
'
