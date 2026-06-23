//! Скан запущенных процессов против блэклиста сигнатур с бэкенда. Best-effort и
//! кроссплатформенно: на Linux читаем /proc, на Windows — tasklist.

use super::handshake::Signature;

/// Обнаружение, найденное сканом (соответствует DetectionInput на бэкенде).
#[derive(Debug, Clone, serde::Serialize)]
pub struct Detection {
    pub source: String,
    #[serde(rename = "type")]
    pub kind: String,
    pub signature: String,
    pub severity: i32,
}

/// Возвращает список найденных совпадений процессов с блэклистом.
pub fn scan_processes(blacklist: &[Signature]) -> Vec<Detection> {
    let process_sigs: Vec<&Signature> = blacklist
        .iter()
        .filter(|s| s.kind == "process" && !s.pattern.is_empty())
        .collect();
    if process_sigs.is_empty() {
        return Vec::new();
    }

    let processes = running_processes();
    let mut detections = Vec::new();
    for proc_name in &processes {
        let lower = proc_name.to_lowercase();
        for sig in &process_sigs {
            if lower.contains(&sig.pattern.to_lowercase()) {
                detections.push(Detection {
                    source: "launcher".to_string(),
                    kind: "process".to_string(),
                    signature: sig.pattern.clone(),
                    severity: sig.severity,
                });
            }
        }
    }
    detections
}

#[cfg(target_os = "linux")]
fn running_processes() -> Vec<String> {
    let mut out = Vec::new();
    let Ok(entries) = std::fs::read_dir("/proc") else {
        return out;
    };
    for entry in entries.flatten() {
        let name = entry.file_name();
        // Каталоги-PID состоят из цифр.
        if !name.to_string_lossy().chars().all(|c| c.is_ascii_digit()) {
            continue;
        }
        let comm_path = entry.path().join("comm");
        if let Ok(comm) = std::fs::read_to_string(&comm_path) {
            let c = comm.trim();
            if !c.is_empty() {
                out.push(c.to_string());
            }
        }
    }
    out
}

#[cfg(target_os = "windows")]
fn running_processes() -> Vec<String> {
    use std::process::Command;
    let mut out = Vec::new();
    let mut tasklist = Command::new("tasklist");
    tasklist.args(["/fo", "csv", "/nh"]);
    crate::hide_console_window(&mut tasklist);
    if let Ok(output) = tasklist.output() {
        let text = String::from_utf8_lossy(&output.stdout);
        for line in text.lines() {
            // Формат CSV: "image.exe","pid",... — берём первое поле.
            if let Some(name) = line.split("\",\"").next() {
                let name = name.trim_matches('"').trim();
                if !name.is_empty() {
                    out.push(name.to_string());
                }
            }
        }
    }
    out
}

#[cfg(not(any(target_os = "linux", target_os = "windows")))]
fn running_processes() -> Vec<String> {
    Vec::new()
}
