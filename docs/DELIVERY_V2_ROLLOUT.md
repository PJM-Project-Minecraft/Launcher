# Production rollout delivery v2

Этот runbook — единственный порядок первого включения delivery v2. Команды
миграции, GC и публикации launcher release не входят в обычный startup/deploy и
выполняются оператором только после прохождения соответствующего gate.

## Зафиксированные кандидаты

- Launcher/Backend: ветка `refactor/delivery-v2`.
- WEB: ветка `refactor/delivery-v2` в отдельном репозитории WEB.
- Первый v2 launcher: `0.5.7`, Linux `linux-x64`, Windows `windows-x64`.
- Backend URL в обоих бинарниках: `https://launcher.likonchik.xyz`.
- Приватный update key остаётся только на release-box.
- Приватный delivery manifest seed находится в production `.env`; в launcher
  вшивается только соответствующий публичный ключ.

Перед фактическим rollout записать immutable SHA обоих merge-коммитов и SHA-256
обоих бинарников в операторский журнал. Не собирать артефакты заново между
canary и mandatory promotion.

Оба production artifact собираются на release-box из одного commit и одним
update key. Docker images используют Ubuntu snapshot `20260801T000000Z` и
Rust `1.96.0`:

```bash
scripts/prod/build-player-launcher-linux.sh \
  --api-url https://launcher.likonchik.xyz \
  --signing-key /secure/pjm-update-signing.key \
  --manifest-pubkey <public-delivery-key> \
  --out-dir release-artifacts/production-candidate-0.5.7
scripts/prod/build-player-launcher-windows.sh \
  --api-url https://launcher.likonchik.xyz \
  --signing-key /secure/pjm-update-signing.key \
  --manifest-pubkey <public-delivery-key> \
  --out-dir release-artifacts/production-candidate-0.5.7
```

Финальный offline gate и единый `SHA256SUMS`:

```bash
scripts/prod/verify-player-launcher-artifacts.sh \
  --dir release-artifacts/production-candidate-0.5.7 \
  --version 0.5.7 \
  --api-url https://launcher.likonchik.xyz \
  --commit "$(git rev-parse HEAD)" \
  --update-pubkey <public-update-key> \
  --manifest-pubkey <public-delivery-key>
```

## 0. Нулевой gate

1. Оба worktree чистые, review завершён.
2. `./dev-check --full` зелёный.
3. WEB: `npm run build` и `CHECK_BASE=... npm run check` зелёные.
4. На VPS не менее `legacy profile bytes + active launcher bytes + 4 GiB`.
5. Последний `launcher-*.sql.gz` моложе 24 часов и проходит `gzip -t`.
6. `/app/storage` является persistent host mount, а не container layer.
7. В `.env` настроены `DELIVERY_*` и публичный `LAUNCHER_UPDATE_PUBKEY`, но
   running services не перезапускаются до отдельного подтверждения rollout.

Бэкап:

```bash
cd /root/Launcher
./scripts/prod/backup-db.sh
gzip -t "$(ls -1t /root/backups/launcher/launcher-*.sql.gz | head -1)"
```

## 1. Deploy только кода Backend

Сначала merge/push подтверждённых коммитов. Затем обычный deploy обновляет
server и bot и выполняет GORM AutoMigrate. Он не запускает backfill и GC.

Сразу после старта:

```bash
cd /root/Launcher
docker compose ps
docker compose logs --tail=100 server bot
scripts/prod/delivery-v2-preflight.sh
```

Preflight проверяет config без печати секретов, backup, mount, диск, наличие
operator binaries и запускает строго read-only `delivery-migrate --dry-run`.
При любой ошибке миграцию не начинать.

## 2. Backfill

```bash
cd /root/Launcher
docker compose exec -T server /app/delivery-migrate --apply \
  | tee "/root/backups/launcher/delivery-v2-migrate-$(date +%Y%m%d-%H%M%S).log"
scripts/prod/delivery-v2-preflight.sh --verify-only
```

Только `--apply` разрешает запись; запуск без флага остаётся dry-run. Команда
идемпотентна. После импорта она сама проверяет подписи manifest и
descriptor, читает каждый CAS chunk и реконструирует каждый активный файл.
До успешного `--verify-only` WEB и launcher `0.5.7` не публиковать.

Дополнительный smoke с JWT тестового пользователя:

- `GET /api/v2/profiles/` возвращает active release;
- manifest SHA-256 и Ed25519 соответствуют headers/snapshot;
- случайный chunk скачивается и совпадает с SHA-256;
- `GET /api/v2/launcher/releases/current` возвращает signed descriptor для
  `linux-x64` и `windows-x64`.

## 3. WEB

Выкатить подтверждённую WEB-ветку только после успешного backfill. Проверить:

- profiles показывают durable jobs и progress после reload;
- release upload возвращает `202`, затем job проходит `queued → running → done`;
- failed job доступен для адресного retry;
- одновременно видны все nonterminal/failed jobs.

## 4. Canary launcher 0.5.7

Загрузить заранее проверенные Linux/Windows binaries и их готовые
`signature.txt`. Сначала `mandatory=false`.

На одном чистом Linux и одном чистом Windows клиенте проверить:

1. обновление 0.5.6 → 0.5.7;
2. restart после self-replace;
3. скачивание выбранного profile с пустым chunk-cache;
4. повторный sync с заполненным cache;
5. обрыв сети в середине chunk и retry;
6. SSE reconnect с HTTP snapshot recovery;
7. запуск Minecraft и anticheat handshake.

Только после этого переключить `mandatory=true`. Не удалять предыдущий release.

## CDN transport rollout (launcher 0.5.10+)

1. Создать `cdn.likonchik.xyz` как DNS-only CNAME на технический домен Timeweb,
   выпустить сертификат и проверить HTTPS без `-k`.
2. Сгенерировать отдельный случайный `DELIVERY_CDN_ORIGIN_SECRET` (минимум 32
   символа). В Timeweb добавить request header
   `X-PJM-Delivery-Origin: <secret>`; CORS, gzip, query string и обработку
   изображений/видео не включать.
3. Сначала выкатить backend-код с пустыми `DELIVERY_CDN_*`: старые и новые
   клиенты продолжат скачивать chunks напрямую.
4. Затем одновременно задать в production `.env`:

   ```dotenv
   DELIVERY_CDN_BASE=https://cdn.likonchik.xyz
   DELIVERY_CDN_ORIGIN_SECRET=<тот же secret, что в Timeweb>
   ```

   и пересоздать только `server`. Bot эти переменные и секрет не получает.
5. Взять существующий hash из активного signed manifest. Прямой запрос к
   `/api/v2/cdn/...` без origin-заголовка обязан дать `403`, а тот же URL через
   `cdn.likonchik.xyz` — `200`, правильные size/SHA-256 и
   `Cache-Control: public, max-age=31536000, immutable`.
6. Выпустить 0.5.10 сначала как optional. Проверить пустой cache, повторный HIT,
   отсутствие `Authorization` на CDN и fallback на backend при временном `503`.

Откат CDN не требует перепубликации manifest: очистить обе `DELIVERY_CDN_*` и
пересоздать `server`. Уже запущенный 0.5.10 при ошибке edge всё равно использует
backend fallback.

## 5. Отключение bridge и GC

`DELIVERY_V1_BRIDGE_UNTIL` отключит v1 автоматически. После подтверждения, что
активных клиентов ниже 0.5.7 нет, выставить `DELIVERY_V1_BRIDGE=false`.

GC запрещён минимум семь суток после cutoff. Первый запуск:

```bash
docker compose exec -T server /app/delivery-gc \
  --keep-profile-releases 3 --grace 168h
```

Перед GC сделать новый backup БД. GC никогда не входит в cron/deploy.

## Rollback

До публикации 0.5.7: вернуть предыдущий code SHA и пересобрать server/bot. Legacy
storage не изменён, v2 tables/CAS можно оставить — старый код их игнорирует.

После optional canary: деактивировать 0.5.7 в admin, не удалять metadata/CAS,
разобрать причину и выпустить новую версию. Не пытаться переподписать или
перезаписать immutable release с тем же version/ID.

После mandatory promotion: backend v2 не откатывать на v1. Снять mandatory у
проблемного release или выпустить исправленную версию выше; bridge держать до
стабилизации. PostgreSQL restore нужен только при повреждении данных, а не для
обычного code rollback.
