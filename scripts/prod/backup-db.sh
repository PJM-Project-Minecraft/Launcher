#!/usr/bin/env bash
#
# Бэкап PostgreSQL из docker compose. Запускается на VPS по cron:
#   0 4 * * * /root/Launcher/scripts/prod/backup-db.sh >> /var/log/launcher-backup.log 2>&1
#
# Хранит KEEP последних дампов в BACKUP_DIR (по умолчанию /root/backups/launcher).
# Восстановление:
#   gunzip -c launcher-XXXX.sql.gz | docker compose exec -T postgres psql -U launcher -d launcher

set -euo pipefail

DIR="${LAUNCHER_DIR:-/root/Launcher}"
BACKUP_DIR="${BACKUP_DIR:-/root/backups/launcher}"
KEEP="${BACKUP_KEEP:-14}"

mkdir -p "${BACKUP_DIR}"
stamp="$(date +%Y%m%d-%H%M%S)"
target="${BACKUP_DIR}/launcher-${stamp}.sql.gz"

cd "${DIR}"
docker compose exec -T postgres pg_dump -U launcher -d launcher | gzip > "${target}"

# Пустой дамп — признак проблемы: не затираем им ротацию.
if [ ! -s "${target}" ]; then
  echo "ERROR: пустой дамп ${target}" >&2
  rm -f "${target}"
  exit 1
fi

# Ротация: оставляем KEEP свежих.
ls -1t "${BACKUP_DIR}"/launcher-*.sql.gz 2>/dev/null | tail -n "+$((KEEP + 1))" | xargs -r rm -f

echo "OK: ${target} ($(du -h "${target}" | cut -f1))"
