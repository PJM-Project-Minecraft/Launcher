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
- Клиент сначала собирает полный staging, затем переключает `files/` через
  журналируемый swap. При падении процесса предыдущая сборка восстанавливается.
- WebSocket/SSE является только сигналом изменения. Долговечное состояние
  читается из HTTP snapshot; после reconnect snapshot перечитывается заново.

## Публикация профиля

1. Администратор сохраняет профиль в WEB.
2. Кнопка «Создать SFTP generation» вызывает
   `POST /api/v2/admin/delivery/profiles/:id/drafts`.
3. Backend создаёт
   `storage/delivery-v2/incoming/profiles/<profile-id>/<generation>.upload`.
4. В `.upload` загружается полный managed-клиент с путями в том виде, в котором
   они должны оказаться внутри `files/` игрока.
5. После завершения загрузки каталог одной SFTP-операцией переименовывается в
   `<generation>.ready`.
6. Watcher атомарно забирает generation в `.processing`, проверяет переносимость
   путей, запрещает symlink, строит chunks, manifest и DB snapshot.
7. Только после одной DB-транзакции новый release становится active. Старый
   остаётся читаемым до явного GC.

Статус находится в `delivery_jobs`, а WEB читает его через
`GET /api/v2/admin/delivery/jobs`. Ошибка публикации сохраняется и generation
переименовывается в `.failed`; успешная — в `.published`.

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

Admin:

- `GET /api/v2/admin/delivery/jobs`
- `POST /api/v2/admin/delivery/profiles/:id/drafts`
- `/api/v2/admin/launcher-releases/*`

## Конфигурация и ключи

Backend:

```dotenv
DELIVERY_ROOT=storage/delivery-v2
DELIVERY_MANIFEST_SIGNING_KEY=<32-byte Ed25519 seed, 64 hex>
DELIVERY_V1_BRIDGE=true
```

В production signing key обязателен. Публичный ключ выводится мигратором и
вшивается в launcher release как `DELIVERY_MANIFEST_PUBKEY`. Отдельный
`LAUNCHER_UPDATE_PUBKEY` проверяет бинарник самообновления.

Пример локальной release-сборки:

```bash
DELIVERY_MANIFEST_PUBKEY=<public-key> \
scripts/prod/build-player-launcher.sh \
  --api-url https://launcher.example.com \
  --signing-key /secure/launcher-update.key
```

## Миграция и отключение v1

Backfill никогда не запускается при старте backend или deploy:

```bash
cd backend
go run ./cmd/delivery-migrate
```

Команда идемпотентно создаёт v2 releases для активных профилей и launcher
artifacts, затем печатает `DELIVERY_MANIFEST_PUBKEY`.

Порядок rollout:

1. Сделать backup БД и storage.
2. Задать signing key и оставить `DELIVERY_V1_BRIDGE=true`.
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

## Проверка до rollout

Минимальный gate:

```bash
./dev-check --full
```

Дополнительно проверить на отдельном окружении: interrupted chunk download,
повреждённый chunk, неверную подпись manifest, crash на каждой фазе swap,
reconnect WEB/SSE и оба launcher artifacts. Никакой из migration/GC/deploy/release
шагов не должен выполняться автоматически.
