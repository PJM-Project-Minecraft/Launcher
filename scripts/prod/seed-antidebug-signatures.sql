-- Сигнатуры процессов HTTP-перехватчиков / отладчиков / реверс-инструментов.
-- Лаунчер (pre-launch + in-game скан) и Java-агент тянут блэклист и репортят совпадения.
--
-- РЕПОРТ-ОНЛИ: severity=5 (< kickSeverity 7 и < autoBanSeverity 8) → сервер НЕ кикает и
-- НЕ банит, только пишет в review-очередь и шлёт Telegram-алерт. Это осознанно: чит-тул
-- легко переименовать (детект по имени процесса — потолок, не стена), а хард-кик ложно
-- выкинул бы своих (QA/стафф с Wireshark/gdb). После обкатки severity можно поднять
-- по конкретной сигнатуре в дашборде до 7+, чтобы включить кик.
--
-- MatchType='word' — по границам слова (ловит 'fiddler' в 'fiddler.exe', но не в
-- 'gdb'→'gdbserver'). Идемпотентно: повторный запуск не плодит дубли.
--
-- Применить на проде:
--   docker compose exec -T postgres psql -U launcher -d launcher < scripts/prod/seed-antidebug-signatures.sql
-- (gen_random_uuid() — встроенная в Postgres 13+, прод на 16.)

INSERT INTO cheat_signatures (id, kind, pattern, match_type, hash_hex, severity, note, enabled, created_at, updated_at)
SELECT gen_random_uuid(), 'process', v.pattern, 'word', '', 5,
       'HTTP-перехватчик/отладчик/реверс — репорт-онли', true, now(), now()
FROM (VALUES
    -- HTTP-перехватчики / снифферы трафика
    ('fiddler'), ('charles'), ('mitmproxy'), ('mitmdump'), ('mitmweb'),
    ('wireshark'), ('tshark'), ('proxifier'), ('httptoolkit'), ('httpdebugger'),
    -- Отладчики
    ('x64dbg'), ('x32dbg'), ('x96dbg'), ('ollydbg'), ('windbg'), ('gdb'), ('lldb'),
    -- Дизассемблеры / реверс
    ('ida'), ('ida64'), ('ghidra'), ('radare2'), ('cheatengine'), ('processhacker'),
    -- Java-декомпиляторы (актуально для читов MC на JVM)
    ('dnspy'), ('recaf'), ('jadx'), ('jd-gui')
) AS v(pattern)
WHERE NOT EXISTS (
    SELECT 1 FROM cheat_signatures s
    WHERE s.kind = 'process' AND s.pattern = v.pattern
);
