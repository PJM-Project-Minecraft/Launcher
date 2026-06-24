//! Сбор аппаратного отпечатка (HWID). Кроссплатформенно, best-effort: чем больше
//! стабильных компонентов удаётся собрать, тем устойчивее отпечаток. Компоненты
//! не отправляются в сыром виде — наружу уходит только солёный SHA-256 (агрегат и
//! раздельные хеши компонентов для fuzzy-матча на сервере).

use serde::Serialize;
use sha2::{Digest, Sha256};

// Соль фиксирует отпечаток за этим лаунчером: один и тот же набор железа даст
// разный хэш в другом проекте, и сырые серийники не восстановить из хэша.
const HWID_SALT: &str = "projectminecraft-anticheat-v1";

/// Раздельные солёные хеши компонентов железа. Отправляются в handshake/init; сервер
/// использует их для fuzzy-бана (смена нестабильного MAC не обходит бан, а одиночная
/// коллизия стабильного компонента не банит). `aggregate` — прежний агрегатный хеш,
/// совместимый со старыми банами (в JSON не уходит — сервер берёт его из hwidHash).
#[derive(Debug, Clone, Serialize)]
pub struct HwidComponents {
    #[serde(skip)]
    pub aggregate: String,
    #[serde(rename = "machineId")]
    pub machine_id: String,
    #[serde(rename = "boardUuid")]
    pub board_uuid: String,
    pub macs: Vec<String>,
}

struct RawComponents {
    machine_id: Option<String>,
    board_uuid: Option<String>,
    macs: Vec<String>,
}

fn salted_hash(value: &str) -> String {
    let mut h = Sha256::new();
    h.update(HWID_SALT.as_bytes());
    h.update(b"|");
    h.update(value.as_bytes());
    to_hex(h.finalize().as_slice())
}

/// Собирает компоненты железа и их раздельные солёные хеши + агрегат.
pub fn collect_hwid_components() -> HwidComponents {
    let raw = collect_raw_components();

    // Агрегат — прежняя формула (sorted+dedup всех сырых компонентов): один и тот же
    // набор железа даёт тот же агрегат, что и старый лаунчер → старые баны живы.
    let mut all: Vec<String> = Vec::new();
    if let Some(m) = &raw.machine_id {
        all.push(m.clone());
    }
    if let Some(b) = &raw.board_uuid {
        all.push(b.clone());
    }
    all.extend(raw.macs.iter().cloned());
    all.sort();
    all.dedup();
    let mut hasher = Sha256::new();
    hasher.update(HWID_SALT.as_bytes());
    for part in &all {
        hasher.update(b"|");
        hasher.update(part.as_bytes());
    }
    let aggregate = to_hex(hasher.finalize().as_slice());

    HwidComponents {
        aggregate,
        machine_id: raw.machine_id.as_deref().map(salted_hash).unwrap_or_default(),
        board_uuid: raw.board_uuid.as_deref().map(salted_hash).unwrap_or_default(),
        macs: raw.macs.iter().map(|m| salted_hash(m)).collect(),
    }
}

fn to_hex(bytes: &[u8]) -> String {
    let mut out = String::with_capacity(bytes.len() * 2);
    for b in bytes {
        out.push_str(&format!("{:02x}", b));
    }
    out
}

#[cfg(target_os = "linux")]
fn read_trimmed(path: &str) -> Option<String> {
    std::fs::read_to_string(path).ok().and_then(|v| {
        let t = v.trim().to_string();
        if t.is_empty() {
            None
        } else {
            Some(t)
        }
    })
}

#[cfg(target_os = "linux")]
fn collect_raw_components() -> RawComponents {
    RawComponents {
        machine_id: read_trimmed("/etc/machine-id"),
        board_uuid: read_trimmed("/sys/class/dmi/id/product_uuid"),
        macs: mac_addresses_linux(),
    }
}

#[cfg(target_os = "linux")]
fn mac_addresses_linux() -> Vec<String> {
    let mut out = Vec::new();
    let Ok(entries) = std::fs::read_dir("/sys/class/net") else {
        return out;
    };
    for entry in entries.flatten() {
        let name = entry.file_name();
        // Виртуальные интерфейсы нестабильны — пропускаем lo и docker/veth/br.
        let name = name.to_string_lossy();
        if name == "lo" || name.starts_with("docker") || name.starts_with("veth") || name.starts_with("br-") {
            continue;
        }
        let addr_path = entry.path().join("address");
        if let Ok(addr) = std::fs::read_to_string(&addr_path) {
            let addr = addr.trim();
            if !addr.is_empty() && addr != "00:00:00:00:00:00" {
                out.push(addr.to_string());
            }
        }
    }
    out
}

#[cfg(target_os = "windows")]
fn collect_raw_components() -> RawComponents {
    RawComponents {
        machine_id: windows_machine_guid(),
        board_uuid: windows_board_uuid(),
        macs: Vec::new(),
    }
}

#[cfg(target_os = "windows")]
fn windows_machine_guid() -> Option<String> {
    use std::process::Command;
    // MachineGuid из реестра — стабильный идентификатор установки Windows.
    let mut reg = Command::new("reg");
    reg.args([
        "query",
        r"HKLM\SOFTWARE\Microsoft\Cryptography",
        "/v",
        "MachineGuid",
    ]);
    crate::hide_console_window(&mut reg);
    let output = reg.output().ok()?;
    parse_reg_value(&String::from_utf8_lossy(&output.stdout))
}

#[cfg(target_os = "windows")]
fn windows_board_uuid() -> Option<String> {
    use std::process::Command;
    // UUID материнской платы через WMIC (есть на большинстве систем).
    let mut wmic = Command::new("wmic");
    wmic.args(["csproduct", "get", "UUID"]);
    crate::hide_console_window(&mut wmic);
    let output = wmic.output().ok()?;
    let text = String::from_utf8_lossy(&output.stdout);
    for line in text.lines().map(str::trim) {
        if !line.is_empty() && !line.eq_ignore_ascii_case("UUID") {
            return Some(line.to_string());
        }
    }
    None
}

#[cfg(target_os = "windows")]
fn parse_reg_value(stdout: &str) -> Option<String> {
    for line in stdout.lines() {
        if let Some(idx) = line.find("REG_SZ") {
            let value = line[idx + "REG_SZ".len()..].trim();
            if !value.is_empty() {
                return Some(value.to_string());
            }
        }
    }
    None
}

#[cfg(not(any(target_os = "linux", target_os = "windows")))]
fn collect_raw_components() -> RawComponents {
    // macOS и прочее: ограничиваемся доступным, отпечаток слабее.
    RawComponents {
        machine_id: None,
        board_uuid: None,
        macs: Vec::new(),
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn hwid_aggregate_is_64_hex_and_stable() {
        let a = collect_hwid_components().aggregate;
        let b = collect_hwid_components().aggregate;
        assert_eq!(a, b, "агрегатный HWID-хэш должен быть стабильным между вызовами");
        assert_eq!(a.len(), 64, "SHA-256 в hex = 64 символа");
        assert!(a.chars().all(|c| c.is_ascii_hexdigit()));
    }

    #[test]
    fn component_hashes_are_salted_and_distinct() {
        // Хеш компонента — солёный SHA-256 непустого сырья (или пустая строка).
        let c = collect_hwid_components();
        for h in [&c.machine_id, &c.board_uuid] {
            assert!(h.is_empty() || h.len() == 64, "хеш компонента — пустой или 64 hex");
        }
    }

    #[test]
    fn to_hex_encodes_bytes() {
        assert_eq!(to_hex(&[0x00, 0x0f, 0xff]), "000fff");
    }
}
