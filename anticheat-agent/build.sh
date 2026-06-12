#!/usr/bin/env bash
# Сборка Java-агента античита без Gradle: чистый javac + jar (зависимостей вне JDK нет).
# Итоговый agent.jar кладётся в backend/data/, откуда его раздаёт бэкенд.
set -euo pipefail

HERE="$(cd "$(dirname "$0")" && pwd)"
REPO_ROOT="$(cd "$HERE/.." && pwd)"
OUT_DIR="$HERE/build"
CLASSES_DIR="$OUT_DIR/classes"
JAR_NAME="anticheat-agent.jar"
DEST="$REPO_ROOT/backend/data/$JAR_NAME"

rm -rf "$OUT_DIR"
mkdir -p "$CLASSES_DIR"

echo "[anticheat-agent] компиляция (JDK $(javac -version 2>&1))"
find "$HERE/src" -name '*.java' > "$OUT_DIR/sources.txt"
# -g:none — без отладочной информации (имён переменных/номеров строк): анти-RE.
javac -g:none --release 17 -d "$CLASSES_DIR" @"$OUT_DIR/sources.txt"

echo "[anticheat-agent] упаковка jar"
jar --create --file "$OUT_DIR/$JAR_NAME" --manifest "$HERE/MANIFEST.MF" -C "$CLASSES_DIR" .

mkdir -p "$(dirname "$DEST")"
cp "$OUT_DIR/$JAR_NAME" "$DEST"
echo "[anticheat-agent] готово: $DEST"
