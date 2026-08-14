# Project Minecraft

Phase 1 skeleton for a Minecraft launcher:

- Slint + Rust desktop launcher
- React + TypeScript UI prototype
- Go + Fiber v3 backend
- GML custom authorization adapter

## Desktop Launcher Run

The launcher is a Rust desktop app built with Slint. Vite is only a legacy UI debug prototype and is not required for the launcher.

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

UI debug only:

```bash
npm run dev:web
```

For a deployed backend, set `LAUNCHER_API_URL` before starting/building the Slint launcher. Browser debug can use `VITE_API_URL`.

For VPS production release flow, use [docs/vps-production.md](docs/vps-production.md).
The player launcher can be built with a baked-in backend URL via:

```bash
scripts/prod/build-player-launcher.sh --api-url https://launcher.example.com
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

The launcher is project-oriented: players do not choose a custom game directory in v1. After login, the desktop app restores the saved JWT from the system keyring, loads active profiles from the backend, downloads the selected project profile into app data, verifies SHA-256 hashes, removes only files managed by the previous local manifest, and launches the profile command without using a shell.

Backend profile files are stored under:

```text
backend/storage/profiles/<profile-slug>/files/
```

In the dashboard this is shown as "Папка профиля". It is generated from the profile name and is only the safe folder name used on the backend.

Upload project-specific files there over SFTP. Client mods go into:

```text
backend/storage/profiles/<profile-folder>/files/mods/
```

Configs go into `files/config/`, shaderpacks into `files/shaderpacks/`, and any other client files should keep the same relative path they need on the player machine.

In the dashboard:

1. Create or edit a profile.
2. Choose the Minecraft version and loader.
3. Click "Подготовить клиент и загрузчик" to download official Minecraft client files into the backend profile folder.
4. Add your project mods/configs over SFTP.
5. Click "Собрать manifest" so the launcher can download and verify everything.

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

Slint does not require Tauri WebKit packages. If `cargo` is not available in your shell, load Rust first:

```bash
. "$HOME/.cargo/env"
```

Windows and Linux builds use the same Slint Rust crate in `launcher-slint/`. Build platform-specific binaries on the target OS or in CI.
