# Project Minecraft

Phase 1 skeleton for a Minecraft launcher:

- Tauri 2 + Rust desktop launcher
- React + TypeScript production UI
- Go + Fiber v3 backend
- GML custom authorization adapter

## Desktop Launcher Run

The launcher is a Tauri 2 desktop app: Rust owns authentication, updates, files,
anticheat and Minecraft launch; React renders the complete player interface.

Development backend:

```bash
cd backend
SERVER_ADDR=127.0.0.1:8080 \
AUTH_PROVIDER_URL=https://pjm.likonchik.xyz/api/gml/auth \
JWT_SECRET=dev-local-launcher-secret \
ADMIN_LOGINS=your-admin-login \
$HOME/.local/toolchains/go/bin/go run ./cmd/server
```

Desktop launcher:

```bash
npm install
npm run dev:launcher
```

Release build:

```bash
npm run build:launcher
```

Browser-only UI preview (uses demo state and does not call the backend):

```bash
npm run dev:web
```

For a deployed backend, set `LAUNCHER_API_URL` before starting/building the Tauri launcher.

For VPS production release flow, use [docs/vps-production.md](docs/vps-production.md).
For local development, the player launcher can be built with a baked-in backend URL via:

```bash
LAUNCHER_UPDATE_PUBKEY=b241c3efdc832ac81a3f61b1347a241644e66e268fa6efe2bfcd578f43e24e6d \
DELIVERY_MANIFEST_PUBKEY=efb831b7209efd9034c93ab89d93e9e6d00830eb9ef5b0d68d55a65bcfa45eca \
scripts/prod/build-player-launcher.sh \
  --api-url https://launcher.example.com \
  --build-only
```

Do not publish the generic build above. Production release требует update signing key и публичный delivery manifest
key. Оба артефакта собираются controlled Docker wrappers с закреплёнными
Ubuntu snapshot и Rust toolchain:

```bash
scripts/prod/build-player-launcher-linux.sh \
  --api-url https://launcher.example.com \
  --signing-key /secure/update-signing.key \
  --manifest-pubkey <64-lowercase-hex>
scripts/prod/build-player-launcher-windows.sh \
  --api-url https://launcher.example.com \
  --signing-key /secure/update-signing.key \
  --manifest-pubkey <64-lowercase-hex>
```

## Admin Dashboard

Админка переехала в проект сайта: `/home/liko/Разработка/WEB`, маршрут
`https://pjm.likonchik.xyz/admin` (код в `app/admin/`). Бэкенд-API админки
(`/api/admin/*`) остаётся здесь, в `backend/internal/adminapi`.

### Покупки сайта

Подтверждённые YooKassa-заказы принимает `POST /api/site/orders` с заголовком
`X-Site-Secret` (`SITE_ORDER_SECRET`). Повтор по `orderId` идемпотентен.
Админ-JWT открывает список/статистику и ручную отметку выдачи:

- `GET /api/admin/orders?status=&q=&from=&to=&page=`;
- `GET /api/admin/orders/stats?from=&to=`;
- `POST /api/admin/orders/:orderId/issue`.

В production `SITE_ORDER_SECRET` обязателен и должен совпадать с
`SITE_BACKEND_SECRET` проекта WEB, но отличаться от остальных секретов.

## Auth Contract

The backend sends this payload to the GML custom auth endpoint:

```json
{
  "Login": "nickname",
  "Password": "password",
  "Totp": "000000"
}
```

On successful provider response, the backend stores/updates the local user and returns a launcher JWT to the client.

## Project Profiles

Профили и самообновление доставляются через Delivery v2: immutable signed
manifest, content-addressed chunks и журналируемую установку. Для публикации WEB
создаёт SFTP generation; полный managed-клиент загружается в `.upload` и только
после завершения атомарно переименовывается в `.ready`. Ручных scan/drift и
таймера тишины в v2 нет.

Архитектура, API, миграция, временный v1 bridge и операторский GC описаны в
[`docs/MANIFEST_PIPELINE.md`](docs/MANIFEST_PIPELINE.md).

NeoForge 1.20.1 uses the legacy `net.neoforged:forge` artifact; newer NeoForge uses `net.neoforged:neoforge`. For older Minecraft versions, use Forge instead.

Useful backend environment variables:

```bash
ADMIN_LOGINS=nickname1,nickname2
PROFILE_STORAGE_ROOT=storage/profiles
```

In `APP_ENV=development`, if `ADMIN_LOGINS` is empty, the first account that logs in becomes `admin`. In production, always set `ADMIN_LOGINS` explicitly.

Launch command templates support these placeholders:

```text
{java} {game_dir} {profile_dir} {login} {uuid} {access_token} {jvm_args}
```

Example:

```text
{java} {jvm_args} -jar client.jar --username {login} --uuid {uuid} --accessToken {access_token} --gameDir {game_dir}
```

## Desktop Platform Notes

Tauri 2 uses the system webview. On Debian/Ubuntu, install its build dependencies first:

```bash
sudo apt install libwebkit2gtk-4.1-dev libayatana-appindicator3-dev librsvg2-dev
```

If `cargo` is not available in your shell, load Rust first:

```bash
. "$HOME/.cargo/env"
```

Windows and Linux builds use the same Rust crate in `src-tauri/` and the same bundled
frontend from `src/`. Build platform-specific binaries on the target OS or in CI.
