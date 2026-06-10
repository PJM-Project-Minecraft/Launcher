# VPS production release

Эта инструкция про выпуск Project Minecraft для игроков на одном VPS:

- backend работает как `systemd` service на `127.0.0.1:8080`;
- публичный домен с HTTPS проксирует запросы на backend;
- игровой сервер использует `authlib-injector` и проверяет сессии на том же backend;
- лаунчер для игроков собирается с продовым API URL внутри бинарника.

Ниже в примерах домен: `https://launcher.example.com`. Замени его на свой.

## 0. Что должно быть на VPS

Минимум:

- Linux VPS с `systemd`;
- домен, A/AAAA-запись которого смотрит на VPS;
- HTTPS reverse proxy: Caddy или Nginx;
- Go версии из `backend/go.mod`;
- Java для Minecraft-сервера;
- Node.js/npm, если dashboard тоже будет на VPS.

Для одного VPS можно оставить SQLite. Для нескольких backend-инстансов нужен
общий стор сессий и БД/Redis; текущая версия хранит игровые сессии в памяти
одного backend-процесса.

## 1. Backend на VPS

Склонируй/обнови проект на VPS и положи `authlib-injector` рядом с backend:

```bash
cd ~/Launcher
mkdir -p backend/data
cp /path/to/authlib-injector-1.2.5.jar backend/data/authlib-injector.jar
```

Установи backend как systemd service:

```bash
scripts/prod/vps-install-backend.sh \
  --public-url https://launcher.example.com \
  --admin-logins YourAdminNick
```

Скрипт:

- создаст `backend/.env.production`;
- соберёт `backend/bin/launcher-backend`;
- установит `/etc/systemd/system/project-minecraft-backend.service`;
- включит автозапуск и перезапустит backend.

Полезные команды:

```bash
sudo systemctl status project-minecraft-backend
sudo journalctl -u project-minecraft-backend -f
sudo systemctl restart project-minecraft-backend
```

## 2. HTTPS reverse proxy

Backend слушает локально `127.0.0.1:8080`. Наружу открывай домен с HTTPS.

### Caddy

```caddy
launcher.example.com {
  reverse_proxy 127.0.0.1:8080
}
```

### Nginx

```nginx
server {
    listen 80;
    server_name launcher.example.com;
    return 301 https://$host$request_uri;
}

server {
    listen 443 ssl http2;
    server_name launcher.example.com;

    ssl_certificate /etc/letsencrypt/live/launcher.example.com/fullchain.pem;
    ssl_certificate_key /etc/letsencrypt/live/launcher.example.com/privkey.pem;

    location / {
        proxy_pass http://127.0.0.1:8080;
        proxy_set_header Host $host;
        proxy_set_header X-Real-IP $remote_addr;
        proxy_set_header X-Forwarded-For $proxy_add_x_forwarded_for;
        proxy_set_header X-Forwarded-Proto $scheme;
    }
}
```

Проверка:

```bash
curl https://launcher.example.com/api/yggdrasil/
curl -I https://launcher.example.com/api/yggdrasil/authlib-injector.jar
```

## 3. Dashboard

Dashboard нужен только администраторам. Если хочешь держать его на VPS:

```bash
scripts/prod/vps-install-dashboard.sh \
  --api-url https://launcher.example.com
```

Он запустится локально на `127.0.0.1:3000`. Его можно открыть отдельным доменом,
например `admin.example.com`, через reverse proxy на `127.0.0.1:3000`.

Команды:

```bash
sudo systemctl status project-minecraft-dashboard
sudo journalctl -u project-minecraft-dashboard -f
```

## 4. Minecraft-сервер

Скопируй `authlib-injector-1.2.5.jar` в папку игрового сервера и запусти:

```bash
scripts/prod/configure-mc-server-authlib.sh \
  --server-dir "/home/minecraft/server" \
  --public-url https://launcher.example.com
```

Скрипт обновит:

- `user_jvm_args.txt`:
  `-javaagent:authlib-injector-1.2.5.jar=https://launcher.example.com/api/v1/integrations/authlib/minecraft`
- `server.properties`:
  `online-mode=true`
  `enforce-secure-profile=false`

После этого перезапусти Minecraft-сервер. В логах должен появиться
`authlib-injector` с URL твоего backend.

Важно: лаунчер, Minecraft-сервер и `authlib-injector` должны смотреть на один и
тот же backend-инстанс. Иначе снова будет split-brain: токен выдан одним
процессом, а сервер проверяет другой.

## 5. Лаунчер для игроков

На машине, где собираешь релизы:

```bash
scripts/prod/build-player-launcher.sh \
  --api-url https://launcher.example.com
```

Скрипт соберёт release-бинарник с продовым URL внутри и положит пакет в:

```text
dist/releases/
```

Игрокам отдавай архив `project-minecraft-launcher-...tar.gz`. На Linux игрок
запускает:

```bash
./run.sh
```

Для Windows собирай тот же crate на Windows или в CI с тем же env:

```powershell
$env:LAUNCHER_DEFAULT_API_URL="https://launcher.example.com"
cargo build --release --manifest-path launcher-slint/Cargo.toml
```

`LAUNCHER_API_URL` всё ещё можно выставить вручную для отладки, но обычным
игрокам это не нужно.

## 6. Финальная проверка

С VPS или со своей машины:

```bash
scripts/prod/health-check.sh \
  --public-url https://launcher.example.com \
  --mc-host minecraft.example.com \
  --mc-port 25565
```

Чеклист перед выдачей игрокам:

- `https://launcher.example.com/api/yggdrasil/` отдаёт JSON;
- `https://launcher.example.com/api/yggdrasil/authlib-injector.jar` отдаёт jar;
- backend service активен;
- Minecraft-сервер запущен с `online-mode=true`;
- в логах Minecraft есть URL твоего backend;
- лаунчер собран через `build-player-launcher.sh`;
- аккаунт администратора указан в `ADMIN_LOGINS`.

## 7. Обновление версии

Обычный цикл обновления на VPS:

```bash
cd ~/Launcher
git pull
scripts/prod/vps-install-backend.sh \
  --public-url https://launcher.example.com \
  --admin-logins YourAdminNick
sudo systemctl restart project-minecraft-backend
```

Если менялся dashboard:

```bash
scripts/prod/vps-install-dashboard.sh \
  --api-url https://launcher.example.com
```

Если менялся desktop launcher:

```bash
scripts/prod/build-player-launcher.sh \
  --api-url https://launcher.example.com
```

## 8. Бэкапы БД

На VPS дампы снимает `scripts/prod/backup-db.sh` (pg_dump из compose-контейнера, gzip, ротация 14 шт.):

```bash
# установка cron (раз в сутки в 04:00)
(crontab -l 2>/dev/null | grep -v backup-db.sh; \
 echo "0 4 * * * /root/Launcher/scripts/prod/backup-db.sh >> /var/log/launcher-backup.log 2>&1") | crontab -

# ручной запуск / проверка
/root/Launcher/scripts/prod/backup-db.sh
ls -lt /root/backups/launcher | head
```

Восстановление из дампа:

```bash
cd /root/Launcher
gunzip -c /root/backups/launcher/launcher-XXXX.sql.gz | docker compose exec -T postgres psql -U launcher -d launcher
```

Дампы лежат на том же VPS — рекомендуется периодически копировать каталог
`/root/backups/launcher` за его пределы (rclone в облако или scp на домашнюю машину):

```bash
scp root@13.140.17.105:/root/backups/launcher/$(ssh root@13.140.17.105 'ls -1t /root/backups/launcher | head -1') ~/backups/
```

Важно: с введением fail-fast проверки секретов в `/root/Launcher/.env` должны быть
`APP_ENV=production` и настоящий `JWT_SECRET` — иначе backend откажется стартовать.
