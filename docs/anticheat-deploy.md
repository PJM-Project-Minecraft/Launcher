# Античит: статус и выкат на прод

Документ про усиление античита 2026-06 (фазы **P0–P6**): что сделано, что НЕ
реализовано, и как безопасно выкатить на прод. Полный план — `~/.claude/plans/senior-dynamic-island.md`.

Прод: `root@176.108.254.89`, `/root/Launcher`, docker compose, backend за nginx на `:8082`,
домен `launcher.likonchik.xyz`. Деплой — `./deploy.sh` (push `origin/main` → на VPS
`git reset --hard origin/main` → `docker compose up -d --build`). **`backend/data/` в git
НЕ входит** — бинарники едут отдельно через `scp`.

> Актуальная модель доверия (2026-08-26): клиентский proof и P5 подтверждают
> присутствие протокола, но не доказывают, что на контролируемом игроком компьютере
> исполняется одобренный бинарник. P5 теперь выдаёт nonce-bound connection lease:
> в enforce-режиме игровой сервер обновляет `(UUID, login, nonce)` каждые 30 секунд,
> а backend ограничивает deadline последним реальным heartbeat агента. Именно это
> подключение отключается не позднее 180 секунд после последнего heartbeat даже при
> полном обрыве backend. Скриншоты — только
> `native_signed`, всегда помечены `trust=client_untrusted`; это телеметрия, не
> криптографическое доказательство состояния экрана.

---

## 1. Что РЕАЛИЗОВАНО

| Фаза | Коммит | Суть | Запушено |
|---|---|---|---|
| P0+P1 | `62d215d` | Починен серверный kick (nonce-баг); server-authoritative severity; rate-limit/дедуп; эскалация банов; SHA-манифест артефактов + сверка в лаунчере | ✅ да |
| P2 | `a7917d8` | Heartbeat-freshness + reaper; версионирование блэклиста + ETag; `GET /api/anticheat/rules`; Java-агент heartbeat-kick + ре-фетч правил | ✅ да |
| P4 | `ef61113` | Нативка GravitGuard-deep: guard-поток анти-инжекта (поллинг модулей), непрерывный anti-debug, Linux LD_PRELOAD; детект чужого `-javaagent`; кросс-сборка `.dll` | ✅ да |
| P3 | `b418381` | Attestation: challenge-response proof-of-agent (эхо challenge, self-hash агента, nativePresent, отсутствие чужих агентов); флаг `ANTICHEAT_REQUIRE_ATTESTATION` | ❌ **нет** (ломает протокол) |
| P6 | `728b227` | Хардненинг секретов (`Validate`); анти-RE: strip debug-строк/символов нативки, `-g:none` agent.jar | ❌ **нет** (нужен `ANTICHEAT_SECRET` в проде) |

Эндпоинты, появившиеся/изменившиеся: `GET /api/anticheat/manifest` (P1), `GET /api/anticheat/rules` (P2),
`/api/anticheat/heartbeat` теперь отдаёт JSON `{action, blacklistVersion}` (P2), `handshake/init`
возвращает `challenge`, `handshake/confirm` принимает `proof` (P3).

---

## 2. Что НЕ РЕАЛИЗОВАНО

### 2.1. P5 — серверный handshake и connection lease

Компонент `anticheat-neoforge/` реализован. Входной challenge проверяется backend по
живой Verified Yggdrasil-сессии и возвращает её nonce. Затем сервер вызывает
`POST /api/anticheat/p5/lease`: renewal проходит только при совпадении UUID, login и
nonce с серверной сессией и отсутствии отзыва. Сетевая ошибка временно fail-open, но
deadline не продлевает; сервер повторяет verify и при восстановлении связи активирует
тот же probation lease. Reconnect сразу заменяет lease, поэтому даже поздний HTTP-ответ
старого соединения не может вытеснить новое. В report-only локальные lease/deadline не
создаются. `ANTICHEAT_P5_ENFORCE` и `ANTICHEAT_P5_SECRET` должны совпадать в env backend
и игрового сервера.

### 2.2. P4 follow-up — превентивные minhook-хуки (Windows-only)

Сейчас анти-инжект — **поллинг** модулей (реактивно, задержка до 5 с). GravitGuard-ядро —
детур-хуки `LdrLoadDll`/`VirtualProtect`/`GetProcAddress`/JNI-`AttachCurrentThread` со
стек-анализом (превентивно). Не сделаны: сабмодуль minhook пуст, нужна Windows тест-петля
и **подпись бинарника** (иначе AV-ложноположительные). См. `anticheat-native/README.md`.

### 2.3. P6 follow-up — офлайн-подпись и строковая обфускация

- **Ed25519-подпись** `agent.jar`/`.so`/`.dll` офлайн-ключом на этапе релиза + проверка
  публичным ключом в лаунчере (поверх SHA-манифеста P1). Защищает даже от компрометации
  бэкенда. Требует хранения приватного ключа вне сервера + правок релиз-флоу.
- **Authenticode** на Windows-бинарях (лаунчер/.dll) — нужен сертификат.
- Строковая обфускация рантайм-строк (маркеры/типы событий) — низкий приоритет (маркеры публичны).

### 2.4. Известные остаточные слабости (честно)

- **Эвристики P4 — report-only** (`module-unknown`, `ld-preload`, `debugger-runtime`): не
  кикают, только пишутся (обкатка против ложных срабатываний). Промоут до kick — см. §6.
- **Allowlist-обход**: инжект, названный `discord_overlay.so`/`nvidia*.dll`, попадёт в
  allowlist нативного guard. Цена защиты от FP; обфускация не лечит (рантайм-декод наблюдаем).
- **HWID спуфится** (machine-id/реестр/MAC) — баны отсеивают ленивых, не целевых.
- **In-memory `Store`** не переживает рестарт backend и не масштабируется на реплики
  (sessions/nonce/heartbeats в памяти одного процесса). Для multi-replica нужен Redis.
- **Attestation P3 и P5** не доказывают исполнение одобренного бинарника на 100%:
  клиент контролирует proof. P5 добавляет server-authoritative presence/liveness lease,
  но аппаратная attestation остаётся отдельной будущей задачей.

---

## 3. Переменные окружения (`/root/Launcher/.env` на VPS)

| Переменная | Дефолт | Назначение |
|---|---|---|
| `ANTICHEAT_SECRET` | — | **ОБЯЗАТЕЛЬНА в проде** (P6): подпись launch-token. Должна быть задана ЯВНО и ОТЛИЧАТЬСЯ от `JWT_SECRET`. Иначе backend не стартует. |
| `ANTICHEAT_REQUIRE_ATTESTATION` | `false` | P3: `true` — confirm без валидного proof отклоняется. Включать ТОЛЬКО после раздачи нового лаунчера. |
| `ANTICHEAT_HEARTBEAT_TIMEOUT` | `90` | P2: окно живости агента (сек); без heartbeat дольше → сессия гасится reaper'ом. |
| `ANTICHEAT_AUTO_BAN` | `false` | Авто-бан при severity ≥ 8 (эскалация temp 7д → perm). |
| `ANTICHEAT_KICK_SEVERITY` | `7` | Порог серверной severity для kick. |
| `ANTICHEAT_AGENT_PATH` | `data/anticheat-agent.jar` | Путь к agent.jar (для раздачи + манифеста). |
| `ANTICHEAT_NATIVE_LINUX` | `data/libanticheat.so` | Путь к нативке Linux. |
| `ANTICHEAT_NATIVE_WIN` | `data/anticheat.dll` | Путь к нативке Windows. |
| `ANTICHEAT_P5_SECRET` | — | 64 lowercase hex, отдельный server-to-server секрет P5; тот же в env игрового сервера. |
| `ANTICHEAT_P5_ENFORCE` | `false` | `true` включает verify + connection lease; без 64-hex `ANTICHEAT_P5_SECRET` production backend не стартует. То же значение обязательно задать игровому серверу. При `false` режим строго report-only и lease-киков нет. |

Сгенерировать секрет: `openssl rand -hex 32`.

> **ВАЖНО (грабли деплоя 2026-06-12):** мало дописать `ANTICHEAT_SECRET` в `.env` —
> `.env` используется лишь для интерполяции `${VAR}` в compose. Переменная попадёт в
> контейнер, только если есть строка в `environment:` сервиса. В `docker-compose.yml`
> теперь есть `ANTICHEAT_SECRET: "${ANTICHEAT_SECRET:-}"` у `server` и `bot` (commit
> fix(deploy)). Без неё `server`/`bot` уходят в crash-loop на `Validate()` в проде.

---

## 4. Сборка артефактов (на дев-машине)

Go на дев-машине только через Docker. Бинарники кладутся в `backend/data/` (в git не идут).

```bash
# agent.jar (нужен JDK 17+; -g:none уже в build.sh)
cd anticheat-agent && ./build.sh

# нативка Linux (.so)
cd anticheat-native && JAVA_HOME=/usr/lib/jvm/java-21-openjdk-amd64 ./build.sh

# нативка Windows (.dll) — кросс-сборка mingw
sudo apt install gcc-mingw-w64-x86-64        # один раз
cd anticheat-native && JAVA_HOME=/usr/lib/jvm/java-21-openjdk-amd64 ./build-win.sh
# (без локального mingw — собрать в контейнере: см. docker debian:bookworm в истории)

# тесты backend
docker run --rm -v "$PWD/backend":/src -w /src golang:1.26-bookworm go test ./...
```

После сборки в `backend/data/` лежат: `anticheat-agent.jar`, `libanticheat.so`, `anticheat.dll`.

---

## 5. Порядок выката на прод

> `deploy.sh` пушит ВЕСЬ локальный `main` (включая незапушенные P3 и P6). Поэтому
> **сначала** подготовь прод (§5.1), иначе backend упадёт на старте.

### 5.0. (Если P3 ещё не нужен) — выкатить только P0/P1/P2/P4/P6

Они уже запушены, кроме P6. P6 безопасен ПОСЛЕ §5.1. P3 (`b418381`) тоже уедет вместе с
`deploy.sh`, но он transition-safe (флаг `false` по умолчанию → старые клиенты проходят confirm).

### 5.1. Подготовка прода (КРИТИЧНО для P6)

На VPS добавить в `/root/Launcher/.env` (если ещё нет):

```bash
ssh root@176.108.254.89
cd /root/Launcher
# проверить наличие; если нет — сгенерировать и дописать:
grep -q '^ANTICHEAT_SECRET=' .env || echo "ANTICHEAT_SECRET=$(openssl rand -hex 32)" >> .env
grep -q '^ANTICHEAT_REQUIRE_ATTESTATION=' .env || echo "ANTICHEAT_REQUIRE_ATTESTATION=false" >> .env
# убедиться, что ANTICHEAT_SECRET != JWT_SECRET
```

Без этого `Validate()` уронит backend на старте (вызывается в `cmd/server` и `cmd/bot`).

### 5.2. Залить бинарники на VPS (не идут через git!)

```bash
scp backend/data/anticheat-agent.jar backend/data/libanticheat.so backend/data/anticheat.dll \
    root@176.108.254.89:/root/Launcher/backend/data/
```

### 5.3. Деплой backend

```bash
./deploy.sh        # тесты → push main → reset --hard на VPS → docker compose up -d --build
```

Проверить, что backend поднялся (если `ANTICHEAT_SECRET` не задан — будет краш-луп):

```bash
ssh root@176.108.254.89 "cd /root/Launcher && docker compose logs --tail=50 server"
```

### 5.4. Новый релиз лаунчера (для P1-сверки и P3-proof у игроков)

P1 (SHA-сверка) и P3 (proof + `-Dac.challenge`) доходят до игроков ТОЛЬКО с новым
бинарником лаунчера (автообновление), не через backend-деплой.

```bash
scripts/prod/build-player-launcher-linux.sh \
  --api-url https://launcher.likonchik.xyz \
  --signing-key /secure/launcher-update.key \
  --manifest-pubkey <public-delivery-key> \
  --out-dir release-artifacts/production-candidate-X.Y.Z
scripts/prod/build-player-launcher-windows.sh \
  --api-url https://launcher.likonchik.xyz \
  --signing-key /secure/launcher-update.key \
  --manifest-pubkey <public-delivery-key> \
  --out-dir release-artifacts/production-candidate-X.Y.Z
scripts/prod/verify-player-launcher-artifacts.sh \
  --dir release-artifacts/production-candidate-X.Y.Z \
  --version X.Y.Z \
  --api-url https://launcher.likonchik.xyz \
  --commit "$(git rev-parse HEAD)" \
  --update-pubkey <public-update-key> \
  --manifest-pubkey <public-delivery-key>
```

Залить релиз через дашборд (Релизы). Для P3 — **поднять минимальную mandatory-версию**, чтобы
заставить всех обновиться (nginx на VPS должен иметь `client_max_body_size 512m` для заливки).

### 5.5. Включить attestation (P3) — ПОСЛЕ раздачи нового лаунчера

Когда все (или mandatory-гейт) на новом лаунчере:

```bash
ssh root@176.108.254.89
cd /root/Launcher
sed -i 's/^ANTICHEAT_REQUIRE_ATTESTATION=.*/ANTICHEAT_REQUIRE_ATTESTATION=true/' .env
docker compose up -d server
```

До включения смотри логи `attestation would fail (transition mode)` — это будущие отказы;
их не должно остаться у легитимных игроков перед включением флага.

### 5.6. Обязательный порядок перехода на lease-P5

1. Сначала выкатить backend с `/api/anticheat/p5/lease`; старый серверный мод продолжит
   использовать `/p5/revoked`.
2. Собрать и установить новый `anticheat-neoforge` server jar, затем новый client jar
   включить в mandatory-сборку.
3. Проверить в логах сервера успешные `/p5/lease` и отсутствие ложных expiry минимум
   на одном полном игровом сеансе и на сценарии reconnect.
4. Только после этого удалять совместимость со старым `/p5/revoked`. При откате мода
   endpoint пока остаётся доступен.

---

## 6. Промоут эвристик P4 из report-only в kick (после обкатки)

`module-unknown` (новый недоверенный модуль) по умолчанию **report-only**. Понаблюдай детекты
в дашборде; если ложных мало — повысь до kick одним из способов:

- сервер: добавить `"module-unknown": 9` в `systemSeverity` (`backend/internal/anticheat/service.go`)
  → severity 9 ≥ kickSeverity → kick; **или**
- агент: в `Agent.java` маппинг поллера для `module-unknown` поменять на `detect("inject", …)`.

Аналогично `ld-preload`. `debugger-runtime` лучше оставить report-only (отладка ≠ всегда чит).

---

## 7. Проверка после деплоя (smoke)

```bash
# манифест целостности отдаётся
curl -s https://launcher.likonchik.xyz/api/anticheat/manifest | jq

# запуск через лаунчер: confirm проходит, /join ок, игрок заходит на сервер
# негатив: запуск без -javaagent (ручная правка) → нет confirm → /join отказ (403)
# дашборд: детекты приходят (module-unknown report-only; маркеры/foreign-agent → kick)
```

Полный health-check стека: `scripts/prod/health-check.sh --public-url https://launcher.likonchik.xyz`.

---

## 8. Откат

```bash
# backend (код)
ssh root@176.108.254.89 "cd /root/Launcher && git reset --hard <старый-commit> && docker compose up -d --build"

# attestation назад в transition
sed -i 's/^ANTICHEAT_REQUIRE_ATTESTATION=.*/ANTICHEAT_REQUIRE_ATTESTATION=false/' /root/Launcher/.env && docker compose up -d server

# бинарники: вернуть прежние .so/.dll/.jar в backend/data/ через scp
```

Бинарники в `backend/data/` не версионируются git — храни предыдущие версии для отката.
