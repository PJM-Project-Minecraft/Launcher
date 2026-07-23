//! Обнаружение отладчика, подключённого к САМОМУ процессу лаунчера. Нативный агент
//! (guard.c) уже непрерывно проверяет отладчик в игровой JVM; здесь закрывается
//! единственная непокрытая цель — процесс лаунчера, через который реверсят протокол
//! античита (чтобы потом «заглушить» его подделкой confirm-proof).
//!
//! РЕПОРТ-ОНЛИ (fail-open): результат уходит детектом на бэкенд (тип "debugger" →
//! серверная severity 6, soft → сервер НЕ кикает), запуск НЕ блокируется. Иначе
//! AV/EDR/краш-репортер, легитимно подцепивший трейсер, ложно выкинул бы игрока.

/// true, если к текущему процессу подключён отладчик/трейсер.
#[cfg(target_os = "linux")]
pub fn debugger_present() -> bool {
    // /proc/self/status: TracerPid != 0 — процесс трассируется (gdb/strace/ptrace).
    let Ok(status) = std::fs::read_to_string("/proc/self/status") else {
        return false;
    };
    for line in status.lines() {
        if let Some(rest) = line.strip_prefix("TracerPid:") {
            return rest.trim().parse::<i64>().map(|pid| pid != 0).unwrap_or(false);
        }
    }
    false
}

#[cfg(target_os = "windows")]
pub fn debugger_present() -> bool {
    // IsDebuggerPresent: флаг BeingDebugged в PEB — ставят все user-mode отладчики,
    // подключённые к процессу. Простейшая надёжная проверка; глубже (порт отладки,
    // NtQueryInformationProcess) уже делает нативный агент в игровой JVM.
    use windows::Win32::System::Diagnostics::Debug::IsDebuggerPresent;
    unsafe { IsDebuggerPresent().as_bool() }
}

#[cfg(not(any(target_os = "linux", target_os = "windows")))]
pub fn debugger_present() -> bool {
    false
}
