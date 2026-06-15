#!/usr/bin/env bash
# Прогон тестов агента без JUnit: компиляция src+test и запуск main-проверок.
set -euo pipefail

HERE="$(cd "$(dirname "$0")" && pwd)"
OUT_DIR="$HERE/build-test"

rm -rf "$OUT_DIR"
mkdir -p "$OUT_DIR"

echo "[anticheat-agent] компиляция src+test ($(javac -version 2>&1))"
find "$HERE/src" "$HERE/test" -name '*.java' > "$OUT_DIR/sources.txt"
javac --release 17 -d "$OUT_DIR" @"$OUT_DIR/sources.txt"

echo "[anticheat-agent] запуск тестов"
java -cp "$OUT_DIR" xyz.projectminecraft.anticheat.AgentResilienceTest

rm -rf "$OUT_DIR"
