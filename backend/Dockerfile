# Сборка backend: два бинарника — server (Fiber API) и bot (Telegram).
# CGO включён из-за SQLite-драйвера (fallback-БД); рантайм на debian-slim (glibc).
FROM golang:1.26-bookworm AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=1 go build -o /out/server ./cmd/server \
 && CGO_ENABLED=1 go build -o /out/bot ./cmd/bot

# JRE для подготовки клиента: backend запускает headless-установщик NeoForge/Forge
# (--install-client) через java. NeoForge 1.21.x требует Java 21, поэтому берём
# Temurin 21 (в bookworm по умолчанию только openjdk-17). Самодостаточный JRE
# копируется в рантайм-образ — без возни с apt-репозиториями Adoptium.
FROM eclipse-temurin:21-jre AS jre

FROM debian:bookworm-slim
# wget нужен для docker compose healthcheck (GET /health).
RUN apt-get update && apt-get install -y --no-install-recommends ca-certificates wget \
 && rm -rf /var/lib/apt/lists/*
COPY --from=jre /opt/java/openjdk /opt/java/openjdk
ENV JAVA_HOME=/opt/java/openjdk
ENV PATH="/opt/java/openjdk/bin:${PATH}"
WORKDIR /app
COPY --from=build /out/server /app/server
COPY --from=build /out/bot /app/bot
# Команда по умолчанию — API-сервер; бот переопределяет command в docker-compose.
CMD ["/app/server"]
