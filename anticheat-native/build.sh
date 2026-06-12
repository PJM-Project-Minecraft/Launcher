#!/usr/bin/env bash
# Сборка нативного JVMTI-агента под Linux (.so) через gcc напрямую (без CMake).
# Для Windows (.dll) используйте CMakeLists.txt на Windows-машине (MSVC/MinGW) —
# см. README. Итоговая библиотека кладётся в backend/data/.
set -euo pipefail

HERE="$(cd "$(dirname "$0")" && pwd)"
REPO_ROOT="$(cd "$HERE/.." && pwd)"
OUT_DIR="$HERE/build"
LIB_NAME="libanticheat.so"
DEST="$REPO_ROOT/backend/data/$LIB_NAME"

# Поиск JAVA_HOME с заголовками jvmti.h.
JH="${JAVA_HOME:-}"
if [ -z "$JH" ] || [ ! -f "$JH/include/jvmti.h" ]; then
  JH="$(dirname "$(dirname "$(readlink -f "$(command -v javac)")")")"
fi
if [ ! -f "$JH/include/jvmti.h" ]; then
  echo "ERROR: не найден jvmti.h. Задайте JAVA_HOME на JDK." >&2
  exit 1
fi
echo "[anticheat-native] JAVA_HOME=$JH"

mkdir -p "$OUT_DIR"
gcc -O2 -fPIC -shared -pthread \
  -I"$JH/include" -I"$JH/include/linux" \
  -o "$OUT_DIR/$LIB_NAME" "$HERE/src/agent.c" "$HERE/src/guard.c"

mkdir -p "$(dirname "$DEST")"
cp "$OUT_DIR/$LIB_NAME" "$DEST"
echo "[anticheat-native] готово: $DEST"
