# anticheat-neoforge — P5: серверно-авторитетный in-game handshake

Последний замок против «заглушки» античита. Мут работает так: клиент подделывает
`/handshake/confirm` → yggdrasil-сессия помечается `Verified` → игрок заходит **любым**
клиентом (см. аудит). Всё клиентское (attestation-proof, нативный агент) читер обходит
на своей машине. **P5 переносит проверку на игровой сервер, которым читер не управляет.**

> ✅ **Компилируется чисто против NeoForge 21.1.233 (MC 1.21.1)** — `gradle build` собирает
> валидный jar (`build/libs/anticheat-neoforge-0.1.0.jar`, mods.toml + классы на месте, 0
> предупреждений). Network-API проверен под эту версию.
>
> ⚠️ **НО в игре НЕ протестирован** (нет игрового рантайма в этом окружении). Обязательно
> **обкатай на dev-сервере в репорт-онли** (`ANTICHEAT_P5_ENFORCE=false`), прежде чем
> включать enforce на проде — иначе баг в хэндшейке кикнет **всех**. Тот же поэтапный
> rollout, что и у прошлых фаз (`ANTICHEAT_REQUIRE_ATTESTATION` тоже включали после обкатки).
>
> Сборка: `cd anticheat-neoforge && gradle build` (Gradle 8.10+, JDK 21). Версия NeoForge —
> в `gradle.properties` (сейчас 21.1.233).

## Протокол (контракт с бэкендом уже реализован и оттестирован)

```
Игрок заходит на игровой сервер
  ├─ Сервер-мод (ServerHandler): при входе шлёт клиенту P5Challenge{nonce} (32 hex),
  │  запускает таймер (напр. 8с).
  ├─ Клиент-мод (ClientHandler): получает challenge, берёт accessToken текущей сессии
  │  (Minecraft.getInstance().getUser().getAccessToken()), считает
  │  proof = HMAC-SHA256(challenge, accessToken) → hex, шлёт серверу P5Response{proof}.
  ├─ Сервер-мод: получил ответ (или сработал таймаут) → POST на бэкенд:
  │      POST {LAUNCHER_API}/api/anticheat/p5/verify
  │      Header: X-AC-P5-Secret: <ANTICHEAT_P5_SECRET>
  │      Body:   {"playerName":"<ник>","challenge":"<nonce>","proof":"<hex|пусто>"}
  │  Бэкенд ищет Verified-сессию игрока, сверяет proof = HMAC(challenge, её accessToken),
  │  отвечает {"allow": bool, "reason": "...", "reportOnly"?: bool}.
  └─ Сервер-мод: allow=false → kick(reason). allow=true → пускаем.
     Таймаут без ответа = пустой proof → бэкенд вернёт mismatch (в enforce = кик).
```

**Честный потолок.** `accessToken` есть и у читера (он логинился), поэтому кастомный
клиент, **переписавший протокол мода**, теоретически ответит верно. Ценность P5 — не
криптографическая невозможность, а **принуждение присутствия**: массовый чит-клиент, не
реализующий канал мода, на входе **кикается**. Вместе с нативным агентом (его грузит
подлинный мод/лаунчер), обфускацией и анти-MITM это резко поднимает планку. Абсолютной
защиты от полностью переписанного клиента клиентско-серверный хэндшейк не даёт — это
известное свойство, а не баг.

## Бэкенд (готово)

- `POST /api/anticheat/p5/verify` — `backend/internal/anticheat/p5.go` (оттестировано).
- Прод `.env`:
  - `ANTICHEAT_P5_SECRET=<длинный случайный секрет>` — общий с игровым сервером. Пуст → P5 выключен.
  - `ANTICHEAT_P5_ENFORCE=false` — **репорт-онли** (пускаем, но пишем `level=ERROR anticheat P5: proof mismatch`).
    В `true` переводить ТОЛЬКО после того, как мод раздан всем и логи чисты.

## Rollout (строго по шагам)

1. Сгенерь секрет: `openssl rand -hex 32`. Пропиши `ANTICHEAT_P5_SECRET` в прод `.env` и в
   конфиг игрового сервера (см. `P5Config`). `ANTICHEAT_P5_ENFORCE=false`. Задеплой бэкенд.
2. Собери мод (`./gradlew build`) под точную версию NeoForge игрового сервера.
3. **Dev-сервер:** поставь серверный мод, зайди клиентом со свежим лаунчером (клиентский
   мод — в профиль, в `mods/`). Проверь: в логах бэкенда для тебя НЕТ `P5: proof mismatch`.
   Попробуй зайти БЕЗ мода/старым клиентом → должен появиться mismatch (в репорт-онли — пускает).
4. Разложи клиентский мод в прод-профиль (SFTP в `backend/storage/profiles/<slug>/files/mods/`
   → «Сканировать файлы» в дашборде, иначе Hash mismatch у игроков). Серверный — на игровой сервер.
5. Дай игрокам обновиться (несколько дней). Следи за логами: mismatch только у тех, кто мимо мода.
6. Когда чисто — `ANTICHEAT_P5_ENFORCE=true`, `docker compose up -d`. Теперь вход без валидного
   хэндшейка = кик. Держи под рукой откат (`ENFORCE=false`), если пойдут ложные кики.

## Сборка и обфускация

`build.gradle` подключает NeoForge и (опционально) ProGuard для обфускации итогового jar
(имена, строки, control-flow). Обязательно **keep**-правила для точек входа NeoForge
(`@Mod`-класс, payload-record'ы, обработчики) и всего, к чему NeoForge обращается рефлексией
/по имени — иначе мод не загрузится. См. `proguard-rules.pro`. После ProGuard **обязательно
перепроверь в игре** (обфускация Java часто ломает миксины/рефлексию в рантайме, не на компиляции).

Раскладка:
```
anticheat-neoforge/
  build.gradle, gradle.properties, settings.gradle
  proguard-rules.pro
  src/main/resources/META-INF/neoforge.mods.toml
  src/main/java/xyz/projectminecraft/anticheat/p5/
    P5Mod.java           — точка входа (@Mod), регистрация payload'ов
    P5Payloads.java      — P5Challenge / P5Response (CustomPacketPayload)
    P5ServerHandler.java — challenge на входе + вызов бэкенда + kick
    P5ClientHandler.java — ответ HMAC(challenge, accessToken)
    P5Crypto.java        — HMAC-SHA256 → hex (одинаково с бэкендом)
    P5Config.java        — LAUNCHER_API, ANTICHEAT_P5_SECRET, timeout
```

> Network-API (PayloadRegistrar / CustomPacketPayload / PacketDistributor) в NeoforGe
> версионно-зависим. Референс написан под 1.21.x-стиль; сверь сигнатуры со своей версией.
