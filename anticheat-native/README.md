# anticheat-native

Нативный JVMTI-агент античита. Загружается в JVM Minecraft через `-agentpath` и:

- доказывает своё присутствие Java-агенту через flag-файл (`-agentpath:lib=<flagfile>`);
- ставит `ClassFileLoadHook` и инспектирует имена загружаемых классов (включая
  bootstrap-классы, недоступные Java-инструментации) на маркеры читов;
- определяет отладчик (`TracerPid` на Linux / `IsDebuggerPresent` на Windows);
- **guard-поток (`guard.c`)**: непрерывный анти-инжект поллингом загруженных модулей
  (новый недоверенный `.dll`/`.so` после baseline → `module-unknown`) + непрерывный
  anti-debug (ловит late-attach) + на Linux детект `LD_PRELOAD`/`LD_AUDIT`. Сигналы идут
  через `.events` → Java-агент → бэкенд.

Anti late-attach (стандартный Attach API) обеспечивается JVM-флагом
`-XX:+DisableAttachMechanism`, который добавляет лаунчер рядом с `-agentpath`.

### Почему поллинг, а не хуки `ntdll!LdrLoadDll` (как в GravitGuard)

GravitGuard-ядро — детур-хуки `LdrLoadDll`/`VirtualProtect`/`GetProcAddress`/JNI-attach
через minhook. Здесь сознательно выбран **поллинг модулей**: он кроссплатформенный,
не требует сторонних библиотек (сабмодуль minhook в GravitGuard пуст) и **не триггерит
антивирусы**. Тот же класс угроз (инжектированная чужая DLL/.so в процессе) ловится
диффом модулей — реактивно (задержка до 5 с) вместо превентивной блокировки.

**Follow-up (не сделано):** превентивные minhook-хуки `LdrLoadDll`/JNI-`AttachCurrentThread`
со стек-анализом (Windows-only). Требуют Windows тест-петли и **подписи бинарника**
(иначе AV-ложноположительные) — см. P6. До подписи держать новые эвристики в report-only.

## Сборка

### Linux (.so)

```bash
JAVA_HOME=/path/to/jdk ./build.sh
# → backend/data/libanticheat.so
```

### Windows (.dll) — кросс-сборка на Linux через mingw-w64

```bash
sudo apt install gcc-mingw-w64-x86-64   # один раз
JAVA_HOME=/path/to/jdk ./build-win.sh
# → backend/data/anticheat.dll  (jni.h/jvmti.h берутся из JDK, win32/jni_md.h — вендорный)
```

`build-win.sh` линкует статически (`-static -static-libgcc`), чтобы DLL не зависела от
`libgcc_s_seh-1.dll` на машине игрока, и проверяет экспорт `Agent_OnLoad` через `objdump`.

### Кроссплатформенно — через CMake

Требуется установленный JDK (`JAVA_HOME` с `include/jvmti.h`). Собирает `agent.c` + `guard.c`.

```bash
cmake -B build
cmake --build build --config Release
# Linux:   build/libanticheat.so
# Windows: build/Release/anticheat.dll  (MSVC) или build/anticheat.dll (MinGW)
```

Готовые библиотеки кладутся в `backend/data/` и раздаются бэкендом по
`GET /api/anticheat/native/{linux|windows}` (пути задаются через
`ANTICHEAT_NATIVE_LINUX` / `ANTICHEAT_NATIVE_WIN`).

## Ограничения (честно)

Агент работает в user-space: нет ring0/драйверов. flag-файл и инжект агентов
спуфятся пропатченным лаунчером. Это поднимает планку, но не делает обход
невозможным без доверенного железа. Подпись бинарников и обфускация — M6.
