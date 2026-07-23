#!/usr/bin/env bash
#
# Выкатка в прод одной командой.
#   ./deploy.sh
#
# Что делает:
#   1) пушит текущую ветку main в GitHub
#   2) на VPS: git fetch + reset --hard origin/main (прод = точная копия GitHub)
#   3) пересобирает и перезапускает изменившиеся контейнеры
#
# Прод-only файлы (.env, docker-compose.override.yml, backend/storage,
# backend/data) в git НЕ входят и НЕ затрагиваются — reset --hard их не трогает.
#
# ВНИМАНИЕ: не редактируй отслеживаемые git'ом файлы прямо на VPS —
# reset --hard их перезатрёт. Все изменения кода/compose идут через ПК → push.

set -euo pipefail

VPS="${DEPLOY_VPS:-root@176.108.254.89}"
DIR="${DEPLOY_DIR:-/root/Launcher}"
BRANCH="${DEPLOY_BRANCH:-main}"

cyan() { printf '\033[36m%s\033[0m\n' "$1"; }
green() { printf '\033[32m%s\033[0m\n' "$1"; }

cyan "→ [0/3] Тесты backend (в Docker)..."
docker run --rm \
  -v "$(cd "$(dirname "$0")" && pwd)/backend":/src -w /src \
  -v launcher_gomodcache:/go/pkg/mod -v launcher_gocache:/root/.cache/go-build \
  golang:1.26-bookworm go test ./...

cyan "→ [1/3] Пуш ветки ${BRANCH} в GitHub..."
git push origin "${BRANCH}"

cyan "→ [2/3] Обновление кода на VPS..."
ssh "${VPS}" "cd ${DIR} && git fetch -q origin && git reset --hard origin/${BRANCH}"

cyan "→ [3/3] Пересборка и перезапуск контейнеров на VPS..."
ssh "${VPS}" "cd ${DIR} && docker compose up -d --build --remove-orphans"

green "✓ Выкачено в прод."
echo "  Логи:    ssh ${VPS} 'cd ${DIR} && docker compose logs -f --tail=50'"
echo "  Статус:  ssh ${VPS} 'cd ${DIR} && docker compose ps'"
echo "  Откат:   ssh ${VPS} 'cd ${DIR} && git reset --hard <старый-commit> && docker compose up -d --build'"
