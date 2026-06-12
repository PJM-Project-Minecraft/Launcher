#!/usr/bin/env bash
# Кросс-сборка нативного агента под Windows (.dll) на Linux через mingw-w64.
# Требуется: sudo apt install gcc-mingw-w64-x86-64. jni.h/jvmti.h копируются из
# локального JDK, win32/jni_md.h — вендорный (include/win32/jni_md.h).
# Итоговый anticheat.dll кладётся в backend/data/ (раздаётся бэкендом, в git не входит).
set -euo pipefail

HERE="$(cd "$(dirname "$0")" && pwd)"
REPO_ROOT="$(cd "$HERE/.." && pwd)"
OUT_DIR="$HERE/build"
INC_WIN="$OUT_DIR/include-win"
DLL="anticheat.dll"
DEST="$REPO_ROOT/backend/data/$DLL"
CC=x86_64-w64-mingw32-gcc

if ! command -v "$CC" >/dev/null 2>&1; then
  echo "ERROR: $CC не найден. Установите: sudo apt install gcc-mingw-w64-x86-64" >&2
  exit 1
fi

# JDK с jni.h/jvmti.h (платформа неважна — берём заголовки, jni_md.h свой).
JH="${JAVA_HOME:-}"
if [ -z "$JH" ] || [ ! -f "$JH/include/jvmti.h" ]; then
  JH="$(dirname "$(dirname "$(readlink -f "$(command -v javac)")")")"
fi
if [ ! -f "$JH/include/jvmti.h" ]; then
  echo "ERROR: не найден jvmti.h. Задайте JAVA_HOME на JDK." >&2
  exit 1
fi
echo "[anticheat-native] JAVA_HOME=$JH (заголовки), CC=$CC"

mkdir -p "$INC_WIN"
cp "$JH/include/jni.h" "$JH/include/jvmti.h" "$INC_WIN/"
cp "$HERE/include/win32/jni_md.h" "$INC_WIN/"

# -static/-static-libgcc — чтобы DLL не зависела от libgcc_s_seh-1.dll на машине игрока.
"$CC" -O2 -shared -static -static-libgcc \
  -I "$INC_WIN" \
  -o "$OUT_DIR/$DLL" "$HERE/src/agent.c" "$HERE/src/guard.c" -lpsapi

mkdir -p "$(dirname "$DEST")"
cp "$OUT_DIR/$DLL" "$DEST"
echo "[anticheat-native] готово: $DEST"

echo "[anticheat-native] проверка экспорта Agent_OnLoad:"
x86_64-w64-mingw32-objdump -p "$OUT_DIR/$DLL" | grep -iE "Agent_OnLoad|libgcc_s_seh" || true
