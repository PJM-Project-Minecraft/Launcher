//! Скан запущенных процессов против блэклиста сигнатур с бэкенда. Best-effort и
//! кроссплатформенно: на Linux читаем /proc, на Windows — tasklist.

use super::handshake::Signature;
use crate::http_client;

/// Обнаружение, найденное сканом (соответствует DetectionInput на бэкенде).
#[derive(Debug, Clone, serde::Serialize)]
pub struct Detection {
    pub source: String,
    #[serde(rename = "type")]
    pub kind: String,
    pub signature: String,
    pub severity: i32,
}

impl Detection {
    /// Детект «к лаунчеру подключён отладчик». severity игнорируется сервером
    /// (systemSeverity["debugger"]=6, confidence soft → не кик, только review/алерт).
    pub fn launcher_debugger() -> Self {
        Self {
            source: "launcher".to_string(),
            kind: "debugger".to_string(),
            signature: "launcher-debugger".to_string(),
            severity: 0,
        }
    }
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
            if process_matches(sig, &lower) {
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

/// Шлёт один детект на бэкенд по launch-token (для in-game скана во время игры).
/// Best-effort: сетевой сбой игнорируется. Severity и confidence сервер пересчитывает сам.
pub fn report_detection(api_url: &str, launch_token: &str, d: &Detection) {
    let Ok(client) = http_client() else {
        return;
    };
    let url = format!("{}/api/anticheat/detect", api_url.trim_end_matches('/'));
    let body = serde_json::json!({
        "launchToken": launch_token,
        "source": "launcher",
        "type": d.kind,
        "signature": d.signature,
        "severity": d.severity,
        "details": { "name": d.signature, "ingame": true },
    });
    let _ = client.post(url).json(&body).send();
}

/// Сопоставляет сигнатуру с именем процесса (нижний регистр) по её match_type.
/// substring — дефолт (обратная совместимость); exact — полное равенство; word — по
/// границам слова (снимает ложняки вроде "java"→"javaw"). regex/hash для скана
/// процессов лаунчером не поддержаны (нет regex-крейта; хеш — не про имена процессов).
/// Это не занижает enforcement: сервер пересчитывает severity по своей сигнатуре.
fn process_matches(sig: &Signature, proc_lower: &str) -> bool {
    let pattern = sig.pattern.to_lowercase();
    if pattern.is_empty() {
        return false;
    }
    match sig.match_type.as_str() {
        "exact" => proc_lower == pattern,
        "word" => matches_word(proc_lower, &pattern),
        "regex" | "hash" => false,
        _ => proc_lower.contains(&pattern), // substring (вкл. пустой match_type)
    }
}

/// true, если needle встречается в haystack как отдельное слово (границы — начало/конец
/// или не-словесный байт). Сравнение побайтовое — безопасно для произвольного UTF-8.
fn matches_word(haystack: &str, needle: &str) -> bool {
    let hay = haystack.as_bytes();
    let pat = needle.as_bytes();
    if pat.is_empty() || pat.len() > hay.len() {
        return false;
    }
    let mut i = 0;
    while i + pat.len() <= hay.len() {
        if &hay[i..i + pat.len()] == pat {
            let left_ok = i == 0 || !is_word_byte(hay[i - 1]);
            let right_ok = i + pat.len() == hay.len() || !is_word_byte(hay[i + pat.len()]);
            if left_ok && right_ok {
                return true;
            }
        }
        i += 1;
    }
    false
}

fn is_word_byte(b: u8) -> bool {
    b.is_ascii_alphanumeric() || b == b'_'
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
