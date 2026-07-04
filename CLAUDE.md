# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## Что это

Монорепо лаунчера Minecraft. Один Go-бэкенд обслуживает лаунчер, админ-дашборд,
Telegram-бота и yggdrasil-аутентификацию игрового сервера.

Компоненты:
- `backend/` — Go + Fiber v3, GORM. Два бинарника: `cmd/server` (API) и `cmd/bot` (Telegram). Делят одну БД.
- `dashboard/` — Next.js 15 (App Router) + Tailwind 4. Только админка.
- `launcher-slint/` — десктоп-лаунчер на Rust + Slint. Текущая версия: `0.3.8` (в `Cargo.toml`).
- `anticheat-native/` — JVMTI-агент на C (`.so`/`.dll`), грузится в JVM Minecraft через `-agentpath`.
- `anticheat-agent/` — Java-агент античита (`-javaagent`), работает в паре с нативным.
- `src/` + Vite — **легаси** React-прототип UI, для лаунчера не нужен (`npm run dev:web`).

Артефакты агентов (`backend/data/`):
- `libanticheat.so`, `anticheat.dll` — нативный JVMTI-агент (собирается через `anticheat-native/build.sh`)
- `anticheat-agent.jar` — Java-агент (собирается через `anticheat-agent/build.sh`)
- `authlib-injector.jar` — для yggdrasil
- `yggdrasil_key.pem` — генерируется автоматически

**Важно:** `backend/data/` и `backend/storage/` в git не входят, на прод едут через `scp` прямо на VPS.

---

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

**Go на дев-машине установлен ТОЛЬКО через Docker.** Если нужен локально:
```bash
docker run --rm \
  -v "$PWD/backend":/src \
  -v launcher_gocache:/root/.cache/go-build \
  -v launcher_gomodcache:/go/pkg/mod \
  -w /src golang:1.26-bookworm go test ./...
```
`deploy.sh` гоняет тесты именно так. Образ: `golang:1.26-bookworm`.

Сборка плеер-лаунчера с зашитым URL бэкенда:
```bash
scripts/prod/build-player-launcher.sh --api-url https://launcher.likonchik.xyz
# либо переменная LAUNCHER_API_URL при cargo build/run
```
Версия бампится в `launcher-slint/Cargo.toml` → обязательно `cargo update -p launcher-slint` после правки.

---

## Деплой

`./deploy.sh` — прод = точная копия `origin/main`. Push в GitHub → на VPS
`git reset --hard origin/main` → `docker compose up -d --build`.

**Не редактируй отслеживаемые git'ом файлы прямо на VPS** — `reset --hard` их перезатрёт.

Прод-only файлы (в git не входят, deploy не трогает): `.env`, `docker-compose.override.yml`,
`backend/storage`, `backend/data`.

**Прод-окружение (с 2026-06-24 — мигрировано на новый сервер):**
- Сервер: `ssh srv-129` (`root@81.88.221.192`), **LXC-контейнер Proxmox** (Debian 12, 4 CPU / 6 ГБ / 69 ГБ), `/root/Launcher`. Docker в LXC работает (nesting). За NAT, внутренний IP `192.168.88.53`.
- Docker compose: `launcher-server-1` (Fiber API, слушает `0.0.0.0:8080` **и** `0.0.0.0:8081` — оба на `:8080` контейнера), `launcher-bot-1` (Telegram long polling), `launcher-postgres-1` (Postgres 16, `127.0.0.1:5432`), `launcher-dashboard-1` (`0.0.0.0:3000`)
- **Нет хостового nginx/certbot.** Домены и SSL — через **Reverse Proxy хостинга** (вкладка панели, автоSSL Let's Encrypt): `pjm.likonchik.xyz` → порт `8080`; `launcher.likonchik.xyz` → порт `8081` (хостинг не даёт два прокси-правила на один порт → второй бинд `8081` у `server`); `admin.likonchik.xyz` → порт `3000`.
- CloudFlare: `launcher`/`pjm`/`admin` — **серое облако (DNS-only)**, A-запись → `81.88.221.192` (хостинг сам выпускает Let's Encrypt; при оранжевом облаке ACME не проходит, а Flexible даёт redirect-петлю). SSL/TLS mode в CF держать **Full**, не Flexible.
- Dashboard: `NEXT_PUBLIC_API_URL=https://launcher.likonchik.xyz` вшит в сборку.
- **Старый сервер** `root@13.140.17.105`: лаунчер-стек остановлен (`docker compose stop`), тома `launcher_postgres_data` оставлены для отката; `amnezia-awg2` (VPN) и `launcher-account-bot-db-1` (MariaDB) там продолжают работать — не трогать.
- Миграция выполнена rsync `/root/Launcher` + `pg_dump`/restore БД (см. [[launcher-prod-deploy]]).

**Гочи деплоя:**
- Порты в base `docker-compose.yml` параметризованы: `${POSTGRES_BIND:-5432}`, `${SERVER_BIND:-8080}`. В прод `.env` задано `POSTGRES_BIND=127.0.0.1:5432` и `SERVER_BIND=127.0.0.1:8082`.
- `docker-compose.override.yml` **добавляет** секции `ports`, а не заменяет — для остального менять в base.
- `.dockerignore` в `backend/` исключает `storage/` и `data/` — монтируются томами, не копируются в образ.
- `docker compose build server` **НЕ пересобирает образ бота** — у них отдельные образы.
- `JWT_SECRET` в base compose только у `server`; боту добавлен через override.
- **ANTICHEAT_SECRET:** мало добавить в `.env` — до контейнера доходит только если есть строка `ANTICHEAT_SECRET: "${ANTICHEAT_SECRET:-}"` в секции `environment:` сервиса. Без неё server+bot уходят в crash-loop на `Validate()`.
- **Диск VPS — только 40G.** При 100% postgres падает (`could not write postmaster.pid`), server/bot — следом (502). После сборок чистить кэш: `docker builder prune -af`.
- **Nginx лимит на аплоад:** для заливки релизов лаунчера через дашборд нужен `client_max_body_size 512m;` в nginx для location бэкенда (:8082). Иначе nginx обрежет multipart-аплоад (BodyLimit 512МБ уже в Fiber, но nginx отрежет первым).
- Git-деплой: VPS тянет deploy-ключом (`~/.ssh/launcher_deploy`, alias `github-launcher` в `~/.ssh/config`). Нужен `git config --global --add safe.directory /root/Launcher` (файлы uid 1000, git под root).
- Откат: `git reset --hard <commit>` на VPS + `docker compose up -d --build`.

**Артефакты агентов в прод** едут через scp (git их не несёт):
```bash
scp backend/data/libanticheat.so   srv-129:/root/Launcher/backend/data/
scp backend/data/anticheat.dll     srv-129:/root/Launcher/backend/data/
scp backend/data/anticheat-agent.jar srv-129:/root/Launcher/backend/data/
```

> **Деплой после миграции (2026-06-24):** `deploy.sh` (git push → `git reset --hard origin/main` на VPS) писался под старый сервер. Новый `srv-129` склонирован на `main` и тянет git напрямую; для авто-деплоя на нём настраивался GitHub Actions self-hosted runner (юзер `github-user`, org `PJM-Project-Minecraft`) — механизм деплоя на новый сервер ещё не финализирован. Прод-only файлы те же (`.env`, `docker-compose.override.yml`, `backend/storage`, `backend/data`). В override `server` добавлен второй порт `8081` (см. прод-окружение).

---

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
- `anticheat` — рычаг enforcement: `confirm` от агента помечает игровую сессию `Verified`.
  Без confirm `/join` возвращает 403 (игрок не попадёт на сервер). Подписывает launch-token
  отдельным `ANTICHEAT_SECRET`. Детекты → HWID-бан + алерты в Telegram.
- `profiles` — проекты-сборки. Файлы профиля: `backend/storage/profiles/<slug>/files/` (моды → `.../files/mods/`).
  Реалтайм через SSE-брокер (`events.NewBroker`) на `/api/profiles/events`.
- `news` — новости из Telegram-канала.
- `adminapi` — управление пользователями для дашборда.
- `launcherrelease` — релизы лаунчера (автообновление). Бинарники в `backend/storage/releases/<version>/<platform>/`,
  заливка через дашборд (multipart, лимит 200 МБ/файл на уровне Fiber). Публичные `/api/launcher/update|download`;
  событие `launcher-release` идёт через общий SSE-брокер профилей. Обязательные релизы: anticheat handshake/init
  отвечает 426 клиентам ниже mandatory-версии (`X-Launcher-Version`).
- `policy` — политика конфиденциальности. Роуты: `GET /api/policy` (текст + version-константа),
  `POST /api/policy/accept` (JWT, записывает PolicyConsent), `GET /privacy` (HTML-страница).
  Enforcement: `POST /api/anticheat/handshake/init` возвращает 451, если согласие не дано; гейты в боте
  блокируют любые действия до принятия политики.
- `bot` / `telegram` / `botconfig` — Telegram-бот (регистрация аккаунтов, TOTP, донат).

Конфиг — `internal/config/config.go`, все переменные через `env(key, fallback)`,
читает `.env` и `backend/.env`. В `APP_ENV=production` `Validate()` **отказывается
стартовать с дев-секретами** (`dev-only-change-me` и т.п.). Ключевые переменные:
`DATABASE_URL` (нет → SQLite), `JWT_SECRET`, `ANTICHEAT_SECRET` (нет → деривируется
из JWT; в продакшне обязан быть явным и отличаться от JWT), `AUTH_MODE`,
`TOKEN_TTL_HOURS` (дефолт 168 = 7 дней), `ADMIN_LOGINS`.

**Гоча SSE:** `net/http` буферизует chunked-ответ — в тестах создаётся ложная задержка
~15с (кратно heartbeat). Тестировать SSE-доставку нужно **сырым TCP-сокетом**
(`net.Dial` + ручной GET), как сделан `profiles/handler_test.go`.

---

## Десктоп-лаунчер (launcher-slint)

Проект-ориентированный: игрок не выбирает game-dir в v1. Поток: логин → JWT в
системном keyring → загрузка активных профилей с бэкенда → скачивание выбранного
профиля в app data → проверка SHA-256 → удаление только файлов из прошлого
локального манифеста → запуск команды профиля **без shell**.

`src/main.rs` — вся логика (HTTP-клиент, манифесты, запуск Java); UI в `ui/app.slint`.
`src/anticheat/` — HWID-сбор и локальное сканирование.
URL бэкенда: env `LAUNCHER_API_URL` или зашитый при сборке `LAUNCHER_DEFAULT_API_URL`.

Автообновление: `src/updater.rs` — проверка при старте / по SSE / раз в 30 мин,
скачивание в `<exe>.update.partial`, SHA-256, самозамена (Linux: rename поверх;
Windows: exe→.old + rename) и перезапуск по кнопке. Версия = `CARGO_PKG_VERSION`.

**Гоча команды запуска профиля:** для модовых загрузчиков (NeoForge/Forge) команда
должна быть СГЕНЕРИРОВАНА `buildAndSaveLaunchCommands` (вызывается в `PrepareClient`).
Ручной шаблон `-jar client.jar` — это ванильный паттерн → `Unable to access jarfile client.jar`,
MC мгновенно выходит. Профили НЕ мигрируются в прод-БД — при пересоздании профиля нужно
запустить «Подготовить клиент» через дашборд.

**В backend-образе есть Temurin 21 JRE** (нужен для `PrepareClient` — headless-установщик
NeoForge через `javaBinary()`). NeoForge 1.21.x требует Java 21. Java 8 (старый Forge 1.12) не добавлена.

**Slint-гочи:**

1. **Layout + width:** `HorizontalLayout`/`VerticalLayout` как один из нескольких детей элемента
   не фиксирует ширину — `width: parent.width` и `min-width: 0px` на дочерних `Text` игнорируются,
   layout берёт ширину по контенту. Чинить **абсолютным позиционированием** с явными `x`/`width`
   (как сделан весь `ui/app.slint`). При заданном `width` у `Text` корректно работает `overflow: elide`.

2. **Скриншот виджета** (машина пользователя, X11 `:1`, есть wmctrl/ffmpeg):
   ```bash
   # 1. cargo install slint-viewer
   # 2. Создать ui/preview.slint с тестовым компонентом; title: "SLINT_PREVIEW_WIDGET"
   #    (не "Project Minecraft Launcher" — иначе wmctrl схватит реальный лаунчер)
   # 3. Запустить через run_in_background: slint-viewer <abs>/ui/preview.slint
   # 4. wmctrl -lG | grep "SLINT_PREVIEW_WIDGET" → x,y,w,h,id
   # 5. wmctrl -i -a <id> && sleep 1.5
   # 6. ffmpeg -f x11grab -video_size 1920x1080 -i :1 -frames:v 1 -update 1 /tmp/full.png
   #    ffmpeg -i /tmp/full.png -vf "crop=W:H:X:Y" /tmp/crop.png
   # 7. pkill -x slint-viewer  ← ТОЛЬКО -x, не -f (иначе убьёт шелл)
   ```
   Ждать освобождения file lock после `cargo build` перед запуском viewer.

---

## Античит — текущее состояние (на 2026-06-13)

Фазы P0–P6 задеплоены в прод 2026-06-12.

**Сделано:**
- **P0** — critfixes: nonce-баг (`IsActiveByNonce`), server-authoritative severity, rate-limit, дедуп детектов, temp(7д)→perm эскалация.
- **P1** — `GET /api/anticheat/manifest` (SHA-256); лаунчер сверяет SHA агентов перед инжектом.
- **P2** — heartbeat-freshness + reaper (`ANTICHEAT_HEARTBEAT_TIMEOUT`); blacklist versioning + ETag/304; `GET /api/anticheat/rules`; Java-агент: heartbeat-kick + ре-фетч /rules.
- **P3** — attestation: challenge в LaunchClaims + init-ответе; `Confirm(token, proof)` + `verifyProof`; флаг `ANTICHEAT_REQUIRE_ATTESTATION` (сейчас `false` — ждёт раздачи 0.3.1).
- **P4** — нативка `guard.c`: поллинг модулей (module-unknown), непрерывный anti-debug, Linux LD_PRELOAD/maps; детект чужого `-javaagent` (foreign-agent→kick).
- **P6** — в прод `config.Validate()` требует явный `ANTICHEAT_SECRET` (≠ JWT, не дев); нативка: debug-логи только под `-DAC_DEBUG`, strip `-s`; agent.jar `-g:none`.

**Осталось:**
- **P5** — NeoForge-мод `anticheat-neoforge/` (in-game agent-handshake + sign-probe). Самый сильный замок против полностью обойдённого клиента. Не начат.
- **Включить attestation:** после раздачи лаунчера 0.3.1 → `ANTICHEAT_REQUIRE_ATTESTATION=true` в прод `.env` → `docker compose up -d`.

**Ключевая архитектурная гоча:**
Авторизация в anticheat handler — **PER-ROUTE**, не `group.Use`. Если переключить на group middleware — `/api/anticheat/*` покроет launch-token-роуты (они проходят по другой схеме).

**Гоча events-файла:** `<flag>.events` не очищался между запусками → Java-поллер перечитывал старые детекты прошлой сессии. Фикс: лаунчер удаляет `.events` перед запуском, нативный агент обнуляет его в `Agent_OnLoad`.

**Детект через ClassFileLoadHook:** при инъекции через `defineClass(null,...)` name=NULL. Нативный агент парсит имя из байткода (`extract_class_name()` в `anticheat-native/src/agent.c`). Массивы (`[L...;`) не гнать через проверку нелегальных имён — символы `[` `;` легальны.

---

## Релизы лаунчера

Текущие версии:
- **0.3.8** (политика конфиденциальности) — собран, НЕ залит (заливка — ручной шаг владельца).
- Версия бампится в `launcher-slint/Cargo.toml` → после правки `cargo update -p launcher-slint`.

Workflow заливки:
1. Сбилдить: `scripts/prod/build-player-launcher.sh --api-url https://launcher.likonchik.xyz`
2. Залить через UI дашборда (раздел «Релизы»), multipart.
3. Перед первой заливкой убедиться, что nginx имеет `client_max_body_size 512m;`.

Гоча enforcement: первый же mandatory-релиз заблокирует launch 426-м ВСЕМ лаунчерам
без `X-Launcher-Version` или ниже порога. Сначала раздать новый как необязательный,
дать игрокам обновиться, потом включить mandatory.

---

## Контракты

GML auth payload (бэкенд → провайдер): `{"Login","Password","Totp"}`.
При успехе сохраняет/обновляет локального юзера и отдаёт JWT лаунчеру.

Античит:
- Нативный агент доказывает присутствие Java-агенту через flag-файл (`-agentpath:lib=<flagfile>`).
- Ставит `ClassFileLoadHook`, ищет маркеры читов в именах классов, детектит отладчик.
- Late-attach блокируется `-XX:+DisableAttachMechanism` (лаунчер добавляет рядом с `-agentpath`).
- Канал нативный→Java для рантайм-детектов: `<flag>.events` (нативный пишет, Java-поллер каждые 2с читает → `/detect`).
- `ANTICHEAT_AUTO_BAN` по умолчанию off — детекты не банят автоматически.

---

## Семантический поиск по коду (Qdrant) — ОБЯЗАТЕЛЬНО

Кодовая база проиндексирована в локальном Qdrant. **Любой поиск «по смыслу»
(«где реализовано X», «как работает Y») начинай со скилла `semantic-code-search`**,
а не с перебора grep/Glob по каталогам:

```bash
/home/liko/Разработка/Qdrant/qdrant.sh search -p launcher "выдача JWT при логине"
```

Выдаёт путь:строку и фрагмент — дальше читай файлы точечно. Grep используй только
для точных идентификаторов/строк, которые уже известны (`issueToken`, `bad_password`).
После крупных изменений переиндексируй: `qdrant.sh index launcher`. Если Qdrant не
запущен (`qdrant.sh status`) — подними его (`qdrant.sh start`), а не откатывайся на grep.
