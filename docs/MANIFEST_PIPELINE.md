# Delivery v2: manifest, CAS и обновления

Delivery v2 — единый контур доставки профилей Minecraft и самообновления
лаунчера. Старые `scan`, `drift`, bundle, покомпонентная загрузка файлов и
периодический polling не являются частью v2.

## Инварианты

- Публикация профиля начинается только после атомарного SFTP rename
  `<generation>.upload` → `<generation>.ready`. Таймер тишины не используется.
- Каждый release immutable. Список файлов, порядок chunks и профильная
  конфигурация фиксируются в manifest schema 2.
- Manifest профиля и launcher descriptor подписываются delivery Ed25519-ключом,
  а сам launcher artifact — отдельной существующей Ed25519-подписью бинарника.
  Release-сборка клиента без обоих публичных ключей запрещена.
- Все данные идут через один content-addressed transfer engine. FastCDC режет
  файлы на chunks 1–8 МиБ (цель 4 МиБ), CAS адресуется по SHA-256.
- Если строгая SHA-256-проверка находит изменения, клиент сначала собирает полный
  staging, затем переключает `files/` через журналируемый swap. Уже актуальное
  дерево не перестраивается. При падении процесса предыдущая сборка
  восстанавливается.
- WebSocket/SSE является только сигналом изменения. Долговечное состояние
  читается из HTTP snapshot; после reconnect snapshot перечитывается заново.
- Launcher descriptor фиксируется один раз при CAS-import. Изменяемая
  channel policy (`active`, `mandatory`) не переписывает подписанный descriptor.

## Публикация профиля

1. Администратор сохраняет профиль в WEB.
2. Для первой сборки кнопка «Создать пустую generation» вызывает
   `POST /api/v2/admin/delivery/profiles/:id/drafts`; в полученный `.upload`
   загружается полный managed-клиент.
3. Для обычного обновления кнопка «Создать из текущей сборки» вызывает
   `POST /api/v2/admin/delivery/profiles/:id/drafts/from-active`. Backend сам
   материализует активный immutable release из CAS в новый `.upload`, исключая
   текущие `preservePaths`. Администратор заменяет или удаляет только изменённые
   файлы; полный клиент повторно по SFTP не передаётся.
   Перед копированием создаётся durable job, закрепляющая source release. Backend собирает дерево в
   server-owned `.seeding` и только после полной проверки атомарно открывает `.upload`. После restart watcher удаляет
   только свои незавершённые `.seeding`/`.upload` и заново материализует их из того же release.
4. Пути внутри `.upload` должны соответствовать их будущему расположению внутри
   `files/` игрока. Черновик независим от исходного release: его изменение не
   меняет уже опубликованный manifest или CAS-данные.
5. После завершения загрузки каталог одной SFTP-операцией переименовывается в
   `<generation>.ready`.
6. Watcher атомарно забирает generation в `.processing`, проверяет переносимость
   путей, запрещает symlink, строит chunks, manifest и DB snapshot.
7. Только после одной DB-транзакции новый release становится active. Старый
   остаётся читаемым до явного GC.

Статус находится в `delivery_jobs`, а WEB читает его через
`GET /api/v2/admin/delivery/jobs`. Ошибка публикации сохраняется и generation
переименовывается в `.failed`; успешная — в `.published`.
Привязка generation к release применяется в той же DB-транзакции, поэтому
restart между commit и rename лишь завершает job, а не создаёт второй release.

Launcher multipart-upload сначала сохраняется как inactive source и возвращает
`202` с durable job. Отдельный watcher чанкует artifacts, фиксирует descriptors
и активирует channel в одной финальной DB-транзакции. `queued/running` job
возобновляется после restart; failed job можно повторить через WEB.

## API v2

Профили, с JWT:

- `GET /api/v2/profiles/`
- `GET /api/v2/profiles/:id/releases/:release/manifest`
- `GET /api/v2/profiles/:id/chunks/:sha256`

Лаунчер, публично:

- `GET /api/v2/launcher/releases/current?platform=linux-x64&from=0.5.6`
- `GET /api/v2/launcher/releases/:release/chunks/:sha256`
- `GET /api/v2/launcher/releases/:release/artifact?platform=linux-x64` — полный
  файл только для браузерной витрины.

CDN origin, только с постоянным заголовком `X-PJM-Delivery-Origin`, который
Timeweb добавляет на плече CDN → backend:

- `GET /api/v2/cdn/profiles/:id/chunks/:sha256`
- `GET /api/v2/cdn/launcher/releases/:release/chunks/:sha256`

Публичная CDN-база не входит в подписанные immutable документы: профиль получает
её в `deliveryBaseUrl` HTTP snapshot, launcher update — в
`X-Delivery-Base-URL`. Поэтому CDN можно отключить без перепубликации releases.
Клиент не отправляет JWT на CDN, проверяет размер/SHA-256 каждого chunk и после
трёх неудачных CDN-попыток использует исходный backend route.

`current` возвращает immutable signed descriptor; рассчитанная для
текущей версии channel policy приходит отдельно в `X-Update-Mandatory`.
Деактивация/удаление в admin лишь снимает release с канала: уже
выданные release-ID chunks и artifact доступны до явного GC после grace.

Admin:

- `GET /api/v2/admin/delivery/jobs`
- `GET /api/v2/admin/delivery/profiles` — все профили, включая inactive, с точным `activeReleaseId`
- `POST /api/v2/admin/delivery/profiles/:id/drafts`
- `POST /api/v2/admin/delivery/profiles/:id/drafts/from-active`
- `/api/v2/admin/launcher-releases/*`
- `POST /api/v2/admin/launcher-releases/:id/retry`

## Конфигурация и ключи

Backend:

```dotenv
DELIVERY_ROOT=storage/delivery-v2
DELIVERY_MANIFEST_SIGNING_KEY=<32-byte Ed25519 seed, 64 hex>
DELIVERY_CDN_BASE=https://cdn.example.com
DELIVERY_CDN_ORIGIN_SECRET=<random secret, минимум 32 символа>
DELIVERY_V1_BRIDGE=true
DELIVERY_V1_BRIDGE_UNTIL=<UTC RFC3339 cutoff, например 2026-09-21T00:00:00Z>
```

Обе `DELIVERY_CDN_*` переменные задаются только вместе. CDN base обязан быть
корневым HTTPS URL. Origin secret хранится в production `.env` и в заголовках
запросов CDN-ресурса; в launcher, manifest и API snapshot он не попадает.

В production signing key обязателен. Публичный ключ выводится мигратором и
вшивается в launcher release как `DELIVERY_MANIFEST_PUBKEY`. Отдельный
`LAUNCHER_UPDATE_PUBKEY` проверяет бинарник самообновления.

Локальный generic script нужен только для development-проверок:

```bash
LAUNCHER_UPDATE_PUBKEY=<test-public-key> \
DELIVERY_MANIFEST_PUBKEY=<test-public-key> \
scripts/prod/build-player-launcher.sh \
  --api-url https://launcher.example.com \
  --build-only
```

Production-артефакты собираются обоими pinned wrappers и проходят
offline verifier из `docs/DELIVERY_V2_ROLLOUT.md`; generic script не публикуется.

## Миграция и отключение v1

Backfill никогда не запускается при старте backend или deploy:

```bash
cd backend
LAUNCHER_UPDATE_PUBKEY=<public-update-key> go run ./cmd/delivery-migrate --apply
```

Команда идемпотентно создаёт v2 releases для активных профилей и launcher
artifacts, затем печатает `DELIVERY_MANIFEST_PUBKEY`.

Порядок rollout:

1. Сделать backup БД и storage.
2. Задать signing key, `DELIVERY_V1_BRIDGE=true` и близкий
   `DELIVERY_V1_BRIDGE_UNTIL`. После cutoff legacy routes отключатся
   автоматически, даже если boolean забыли снять.
3. Запустить мигратор вручную и проверить manifests/chunks на тестовом клиенте.
4. Выпустить mandatory launcher v2.
5. После подтверждённого перехода клиентов установить
   `DELIVERY_V1_BRIDGE=false`. Это снимает старые profile manifest/files/bundle,
   scan/drift/build и `/api/launcher/update|download` маршруты.

## Retention и rollback

GC запускается только оператором. По умолчанию он сохраняет минимум три новых
profile releases и не трогает недоступные данные моложе семи суток:

```bash
cd backend
go run ./cmd/delivery-gc --keep-profile-releases 3 --grace 168h
```

Rollback профиля — повторная публикация нужного полного дерева как новой
generation. Immutable release не редактируется на месте. До запуска GC старые
manifest/chunks продолжают обслуживать клиентов, которые уже получили snapshot.
Launcher GC после того же grace удаляет CAS references, исходные full
binaries, metadata и завершённый job.

## Проверка до rollout

Полный production-порядок, canary и rollback описаны в
[`DELIVERY_V2_ROLLOUT.md`](DELIVERY_V2_ROLLOUT.md).

Минимальный gate:

```bash
./dev-check --full
```

Дополнительно проверить на отдельном окружении: interrupted chunk download,
повреждённый chunk, неверную подпись manifest, crash на каждой фазе swap,
reconnect WEB/SSE и оба launcher artifacts. Никакой из migration/GC/deploy/release
шагов не должен выполняться автоматически.
