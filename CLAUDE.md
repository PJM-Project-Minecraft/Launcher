# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## Что это

Монорепо лаунчера Minecraft с серверной частью, веб-админкой, десктоп-лаунчером и
системой античита. Один и тот же Go-бэкенд обслуживает лаунчер, админ-дашборд,
Telegram-бота и yggdrasil-аутентификацию игрового сервера.

Компоненты:
- `backend/` — Go + Fiber v3, GORM. Два бинарника: `cmd/server` (API) и `cmd/bot` (Telegram). Делят одну БД.
- `dashboard/` — Next.js 15 (App Router) + Tailwind 4. Только админка.
- `launcher-slint/` — десктоп-лаунчер на Rust + Slint (это «настоящий» лаунчер).
- `anticheat-native/` — JVMTI-агент на C (`.so`/`.dll`), грузится в JVM Minecraft через `-agentpath`.
- `anticheat-agent/` — Java-агент античита (`-javaagent`), работает в паре с нативным.
- `src/` + Vite — **легаси** React-прототип UI, для лаунчера не нужен (`npm run dev:web`).

## Команды

Запуск всего dev-стека одной командой (Docker Postgres + backend + bot + dashboard):
```bash
./dev.sh
```
Без Docker бэкенд автоматически падает на SQLite (`data/launcher.db`).

Отдельные части:
```bash
npm run dev:launcher      # cargo run -p launcher-slint (десктоп-лаунчер)
npm run build:launcher    # cargo build --release
npm run dev:dashboard     # next dev на 127.0.0.1:3000
npm run build:dashboard   # next build (включает проверку типов; eslint не настроен)
```

Бэкенд вручную:
```bash
cd backend && go run ./cmd/server      # API
cd backend && go run ./cmd/bot         # Telegram-бот
```

Тесты и проверки (то же, что гоняет CI в `.github/workflows/ci.yml`):
```bash
cd backend && go vet ./... && go test ./...
cd backend && go test ./internal/anticheat/ -run TestX   # один пакет/тест
cd dashboard && npm run build                            # тайпчек дашборда
```

**Go на дев-машине ставится только через Docker** (см. memory). `deploy.sh` гоняет
тесты в `golang:1.26-bookworm`. Если локального `go` нет — используй тот же образ:
```bash
docker run --rm -v "$PWD/backend":/src -w /src golang:1.26-bookworm go test ./...
```

Сборка плеер-лаунчера с зашитым URL бэкенда:
```bash
scripts/prod/build-player-launcher.sh --api-url https://launcher.example.com
# либо переменная LAUNCHER_API_URL при cargo build/run
```

## Деплой

`./deploy.sh` — прод = точная копия `origin/main`. Push в GitHub → на VPS
`git reset --hard origin/main` → `docker compose up -d --build`.

**Не редактируй отслеживаемые git'ом файлы прямо на VPS** — `reset --hard` их
перезатрёт. Прод-only файлы (`.env`, `docker-compose.override.yml`,
`backend/storage`, `backend/data`) в git не входят и не затрагиваются.
Прод: `13.140.17.105`, `/root/Launcher`, backend за nginx на `:8082`,
домен `launcher.likonchik.xyz` (см. memory `launcher-prod-deploy`).

## Архитектура бэкенда

`cmd/server/main.go` — точка сборки: грузит `config`, открывает БД, гоняет
**GORM AutoMigrate** (raw-SQL миграции в `backend/migrations/` больше не
накатываются — схема ведётся моделями в `internal/models`), регистрирует роуты
каждого домена. Бэкенд стоит за nginx, реальный IP берётся из `X-Forwarded-For`.

Доменные пакеты в `internal/` (каждый: `service.go` логика + `handler.go` роуты):
- `auth` — логин, выдача JWT. `AUTH_MODE=local` валидирует в общей БД (bcrypt + TOTP),
  `http` — внешний GML-провайдер (легаси-совместимость). Брутфорс-лимитер на login-эндпоинтах.
- `yggdrasil` — реализация Yggdrasil API для игрового сервера (authlib-injector).
  Хранит игровые сессии; ключи в `data/yggdrasil_key.pem` (создаются автоматически).
- `anticheat` — связан с yggdrasil-store: **рычаг enforcement** в том, что
  `confirm` от агента помечает игровую сессию `Verified`. Без подтверждения
  античита сессия не верифицируется. Подписывает короткоживущие launch-token
  отдельным `ANTICHEAT_SECRET`. Детекты → HWID-бан + алерты в Telegram.
- `profiles` — проекты-сборки. Файлы профиля на диске:
  `backend/storage/profiles/<slug>/files/` (моды → `.../files/mods/`).
  Реалтайм-обновления через SSE-брокер (`events.NewBroker`) на `/api/profiles/events`.
  Гоча SSE: `net/http` буферизует — тестировать сырым TCP (см. memory `sse-realtime-profiles`).
- `news` — новости из Telegram-канала. `adminapi` — управление пользователями для дашборда.
- `launcherrelease` — релизы лаунчера (автообновление). Бинарники в
  `backend/storage/releases/<version>/<platform>/`, заливка через дашборд
  (multipart, лимит 200 МБ/файл — следи за client_max_body_size в nginx на VPS).
  Публичные `/api/launcher/update|download`; событие `launcher-release` идёт
  через общий SSE-брокер профилей. Обязательные релизы: anticheat handshake/init
  отвечает 426 клиентам ниже минимальной mandatory-версии (X-Launcher-Version).
- `bot` / `telegram` / `botconfig` — Telegram-бот (регистрация аккаунтов, TOTP, донат).

Конфиг — `internal/config/config.go`, все переменные через `env(key, fallback)`,
читает `.env` и `backend/.env`. В `APP_ENV=production` `Validate()` **отказывается
стартовать с дев-секретами** (`dev-only-change-me` и т.п.). Ключевые переменные:
`DATABASE_URL` (нет → SQLite), `JWT_SECRET`, `ANTICHEAT_SECRET` (нет → деривируется
из JWT), `AUTH_MODE`, `TOKEN_TTL_HOURS` (дефолт 168 = 7 дней), `ADMIN_LOGINS`.

## Десктоп-лаунчер (launcher-slint)

Проект-ориентированный: игрок не выбирает game-dir в v1. Поток: логин → JWT в
системном keyring → загрузка активных профилей с бэкенда → скачивание выбранного
профиля в app data → проверка SHA-256 → удаление только файлов из прошлого
локального манифеста → запуск команды профиля **без shell**.

`src/main.rs` — вся логика (HTTP-клиент, манифесты, запуск Java); UI в `ui/app.slint`.
`src/anticheat/` — HWID-сбор и локальное сканирование. URL бэкенда: env
`LAUNCHER_API_URL` или зашитый при сборке `LAUNCHER_DEFAULT_API_URL`.

Автообновление: `src/updater.rs` — проверка при старте/по SSE/раз в 30 мин,
скачивание в `<exe>.update.partial`, SHA-256, самозамена (Linux: rename поверх;
Windows: exe→.old + rename) и перезапуск по кнопке. Версия = `CARGO_PKG_VERSION`.

Slint-гочи (см. memory): layout как не-единственный child не фиксирует ширину
(чинить абсолютным позиционированием); скриншот виджета — `slint-viewer` + `wmctrl` + ffmpeg.

## Контракты

GML auth payload (бэкенд → провайдер): `{"Login","Password","Totp"}`.
При успехе сохраняет/обновляет локального юзера и отдаёт JWT лаунчеру.

Античит: нативный агент доказывает присутствие Java-агенту через flag-файл
(`-agentpath:lib=<flagfile>`), ставит `ClassFileLoadHook`, ищет маркеры читов в
именах классов, детектит отладчик. Late-attach блокируется
`-XX:+DisableAttachMechanism` (лаунчер добавляет рядом с `-agentpath`).
