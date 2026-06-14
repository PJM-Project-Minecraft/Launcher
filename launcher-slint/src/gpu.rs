//! Выбор дискретной видеокарты при запуске игры на Linux (гибридная графика).
//!
//! На ноутбуках с PRIME/Optimus Minecraft по умолчанию рендерится на встроенной
//! GPU. Чтобы задействовать дискретную, процессу нужно выставить переменные
//! окружения: NVIDIA Optimus offload для проприетарного драйвера или `DRI_PRIME=1`
//! для Mesa (AMD/Intel). Логика решения вынесена в чистую `decide_offload`, чтобы
//! её можно было покрыть тестами без реального железа.

#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub enum GpuVendor {
    Nvidia,
    Amd,
    Intel,
    Other,
}

#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub struct Gpu {
    pub vendor: GpuVendor,
}

/// Результат: какие переменные окружения выставить и как назвать видеокарту в UI.
#[derive(Debug, Clone, PartialEq, Eq)]
pub struct GpuOffload {
    pub vendor_label: &'static str,
    pub env: Vec<(&'static str, &'static str)>,
}

/// Решает, нужно ли перенаправлять рендер на дискретную GPU.
///
/// Действует только на гибридных системах (≥2 GPU). Если присутствует NVIDIA —
/// offload работает лишь при загруженном проприетарном драйвере, иначе ничего не
/// делаем (на nouveau/без драйвера переменные NVIDIA сломали бы GLX). Для прочих
/// гибридов (AMD/Intel) используем `DRI_PRIME=1`.
// На не-Linux вызывается только из тестов (offload — фича Linux).
#[cfg_attr(not(target_os = "linux"), allow(dead_code))]
pub fn decide_offload(gpus: &[Gpu], nvidia_driver_loaded: bool) -> Option<GpuOffload> {
    // Дискретная vs встроенная имеет смысл только на гибридных системах.
    if gpus.len() < 2 {
        return None;
    }

    if gpus.iter().any(|g| g.vendor == GpuVendor::Nvidia) {
        if !nvidia_driver_loaded {
            return None;
        }
        return Some(GpuOffload {
            vendor_label: "NVIDIA",
            env: vec![
                ("__NV_PRIME_RENDER_OFFLOAD", "1"),
                ("__GLX_VENDOR_LIBRARY_NAME", "nvidia"),
                ("__VK_LAYER_NV_optimus", "NVIDIA_only"),
            ],
        });
    }

    // Прочие гибриды (AMD/Intel): Mesa выбирает вторую GPU по DRI_PRIME.
    let label = if gpus.iter().any(|g| g.vendor == GpuVendor::Amd) {
        "AMD"
    } else {
        "Intel"
    };
    Some(GpuOffload {
        vendor_label: label,
        env: vec![("DRI_PRIME", "1")],
    })
}

/// Разбирает GPU из sysfs-полей `class` и `vendor` (например `0x030000`, `0x10de`).
/// Возвращает `None`, если устройство не является видеоконтроллером.
#[cfg_attr(not(target_os = "linux"), allow(dead_code))]
pub fn gpu_from_sysfs(class: &str, vendor: &str) -> Option<Gpu> {
    // PCI class display controller: 0x03xxxx (VGA 0x0300, 3D 0x0302, ...).
    let class = class.trim().trim_start_matches("0x");
    if !class.starts_with("03") {
        return None;
    }
    let vendor = vendor.trim().trim_start_matches("0x").to_lowercase();
    let vendor = match vendor.as_str() {
        "10de" => GpuVendor::Nvidia,
        "1002" => GpuVendor::Amd,
        "8086" => GpuVendor::Intel,
        _ => GpuVendor::Other,
    };
    Some(Gpu { vendor })
}

/// Определяет доступный offload на дискретную GPU для текущей машины.
/// Возвращает `None`, если система не гибридная либо offload неприменим.
/// Только Linux; на других ОС всегда `None`.
#[cfg(target_os = "linux")]
pub fn detect_offload() -> Option<GpuOffload> {
    decide_offload(&scan_pci_gpus(), nvidia_driver_loaded())
}

#[cfg(not(target_os = "linux"))]
pub fn detect_offload() -> Option<GpuOffload> {
    None
}

#[cfg(target_os = "linux")]
fn scan_pci_gpus() -> Vec<Gpu> {
    let mut gpus = Vec::new();
    let Ok(entries) = std::fs::read_dir("/sys/bus/pci/devices") else {
        return gpus;
    };
    for entry in entries.flatten() {
        let path = entry.path();
        let class = std::fs::read_to_string(path.join("class")).unwrap_or_default();
        let vendor = std::fs::read_to_string(path.join("vendor")).unwrap_or_default();
        if let Some(g) = gpu_from_sysfs(&class, &vendor) {
            gpus.push(g);
        }
    }
    gpus
}

#[cfg(target_os = "linux")]
fn nvidia_driver_loaded() -> bool {
    std::path::Path::new("/proc/driver/nvidia/version").exists()
}

#[cfg(test)]
mod tests {
    use super::*;

    fn gpu(vendor: GpuVendor) -> Gpu {
        Gpu { vendor }
    }

    #[test]
    fn single_gpu_does_nothing() {
        assert_eq!(decide_offload(&[gpu(GpuVendor::Nvidia)], true), None);
        assert_eq!(decide_offload(&[gpu(GpuVendor::Amd)], false), None);
    }

    #[test]
    fn empty_does_nothing() {
        assert_eq!(decide_offload(&[], true), None);
    }

    #[test]
    fn nvidia_hybrid_with_driver_uses_offload() {
        let result = decide_offload(&[gpu(GpuVendor::Intel), gpu(GpuVendor::Nvidia)], true)
            .expect("ожидался NVIDIA offload");
        assert_eq!(result.vendor_label, "NVIDIA");
        assert!(result.env.contains(&("__NV_PRIME_RENDER_OFFLOAD", "1")));
        assert!(result.env.contains(&("__GLX_VENDOR_LIBRARY_NAME", "nvidia")));
        assert!(result.env.contains(&("__VK_LAYER_NV_optimus", "NVIDIA_only")));
    }

    #[test]
    fn nvidia_hybrid_without_driver_does_nothing() {
        assert_eq!(
            decide_offload(&[gpu(GpuVendor::Intel), gpu(GpuVendor::Nvidia)], false),
            None
        );
    }

    #[test]
    fn amd_hybrid_uses_dri_prime() {
        let result = decide_offload(&[gpu(GpuVendor::Amd), gpu(GpuVendor::Amd)], false)
            .expect("ожидался DRI_PRIME");
        assert_eq!(result.vendor_label, "AMD");
        assert_eq!(result.env, vec![("DRI_PRIME", "1")]);
    }

    #[test]
    fn intel_igpu_with_amd_dgpu_uses_dri_prime() {
        let result = decide_offload(&[gpu(GpuVendor::Intel), gpu(GpuVendor::Amd)], false)
            .expect("ожидался DRI_PRIME");
        assert_eq!(result.env, vec![("DRI_PRIME", "1")]);
    }

    #[test]
    fn parses_vga_and_3d_controllers() {
        assert_eq!(
            gpu_from_sysfs("0x030000", "0x10de"),
            Some(Gpu { vendor: GpuVendor::Nvidia })
        );
        assert_eq!(
            gpu_from_sysfs("0x030200", "0x1002"),
            Some(Gpu { vendor: GpuVendor::Amd })
        );
        assert_eq!(
            gpu_from_sysfs("0x030000", "0x8086"),
            Some(Gpu { vendor: GpuVendor::Intel })
        );
    }

    #[test]
    fn ignores_non_display_devices() {
        // Аудиоконтроллер (0x0403) — не видеокарта.
        assert_eq!(gpu_from_sysfs("0x040300", "0x10de"), None);
    }
}
