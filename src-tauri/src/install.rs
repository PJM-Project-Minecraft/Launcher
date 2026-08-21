use std::fs::{self, File};
use std::io::{Read, Write};
use std::path::{Path, PathBuf};

use sha2::{Digest, Sha256};

const USERS_DIR: &str = "users";
const MIGRATION_DIR: &str = ".pjm-migration";
const MIGRATION_BACKUP_DIR: &str = ".pjm-migration-old-users";
const MIGRATION_MARKER: &str = ".pjm-migration-source";

#[derive(Debug)]
pub(crate) struct MigrationResult {
    pub(crate) destination: PathBuf,
    pub(crate) copied_existing_data: bool,
}

/// Возвращает выбранный корень установки. Старые settings.json без поля
/// `installRoot` продолжают использовать системный data-dir.
pub(crate) fn configured_root(value: Option<&str>, default_root: &Path) -> Result<PathBuf, String> {
    let Some(value) = value.map(str::trim).filter(|value| !value.is_empty()) else {
        return Ok(default_root.to_path_buf());
    };
    let path = PathBuf::from(value);
    if !path.is_absolute() {
        return Err("Папка установки в settings.json должна быть абсолютным путём.".to_string());
    }
    Ok(path)
}

/// Копирует все пользовательские профили в НОВУЮ пустую папку на другом диске.
/// Исходник намеренно не удаляется: пока пользователь не проверил запуск с нового
/// диска, старая папка остаётся безопасной резервной копией.
pub(crate) fn migrate_users(
    source_root: &Path,
    destination: &Path,
) -> Result<MigrationResult, String> {
    fs::create_dir_all(destination)
        .map_err(|err| format!("Не удалось создать новую папку установки: {err}"))?;

    let source = canonical_or_absolute(source_root)?;
    let destination = destination
        .canonicalize()
        .map_err(|err| format!("Не удалось открыть новую папку установки: {err}"))?;

    if source == destination {
        return Ok(MigrationResult {
            destination,
            copied_existing_data: false,
        });
    }
    if destination.starts_with(&source) || source.starts_with(&destination) {
        return Err(
            "Новая папка не должна находиться внутри старой папки установки (или наоборот)."
                .to_string(),
        );
    }
    let retry = retry_destination_matches(&destination, &source)?;
    if !retry && directory_has_entries(&destination)? {
        return Err("Выберите пустую папку для переноса файлов.".to_string());
    }

    // Маркер делает перенос повторяемым. При повторе данные копируются заново:
    // после неудачной записи settings.json игрок мог ещё запускать старую копию,
    // поэтому уже подготовленный destination/users мог успеть устареть.
    let marker_path = destination.join(MIGRATION_MARKER);
    let mut marker = File::create(&marker_path)
        .map_err(|err| format!("Не удалось подготовить перенос: {err}"))?;
    marker
        .write_all(source.to_string_lossy().as_bytes())
        .and_then(|_| marker.sync_all())
        .map_err(|err| format!("Не удалось сохранить маркер переноса: {err}"))?;
    sync_directory(&destination)?;
    let destination_users = destination.join(USERS_DIR);
    let staging = destination.join(MIGRATION_DIR);
    let backup_users = destination.join(MIGRATION_BACKUP_DIR);
    if retry && backup_users.is_dir() && !destination_users.exists() {
        fs::rename(&backup_users, &destination_users)
            .map_err(|err| format!("Не удалось восстановить прерванный перенос: {err}"))?;
    }
    if retry && staging.exists() {
        fs::remove_dir_all(&staging)
            .map_err(|err| format!("Не удалось очистить незавершённый перенос: {err}"))?;
    }
    if retry && backup_users.exists() {
        fs::remove_dir_all(&backup_users)
            .map_err(|err| format!("Не удалось очистить предыдущую копию переноса: {err}"))?;
    }

    let source_users = source.join(USERS_DIR);
    if !source_users.exists() {
        fs::create_dir_all(&destination_users)
            .map_err(|err| format!("Не удалось подготовить новую папку: {err}"))?;
        return Ok(MigrationResult {
            destination,
            copied_existing_data: false,
        });
    }

    let staged_users = staging.join(USERS_DIR);
    let result: Result<(), String> = (|| {
        copy_tree(&source_users, &staged_users)?;
        if destination_users.exists() {
            fs::rename(&destination_users, &backup_users)
                .map_err(|err| format!("Не удалось обновить подготовленную копию: {err}"))?;
        }
        if let Err(err) = fs::rename(&staged_users, &destination_users) {
            if backup_users.exists() && !destination_users.exists() {
                let _ = fs::rename(&backup_users, &destination_users);
            }
            return Err(format!("Не удалось завершить перенос профилей: {err}"));
        }
        if backup_users.exists() {
            fs::remove_dir_all(&backup_users)
                .map_err(|err| format!("Не удалось очистить предыдущую копию: {err}"))?;
        }
        sync_directory(&destination)?;
        Ok(())
    })();

    if result.is_err() {
        // Удаляется только созданная нами служебная staging-папка внутри заранее
        // проверенного пустого destination. Исходные данные не затрагиваются.
        let _ = fs::remove_dir_all(&staging);
    } else {
        let _ = fs::remove_dir(&staging);
    }
    result?;

    Ok(MigrationResult {
        destination,
        copied_existing_data: true,
    })
}

/// Вызывается только после успешного сохранения `installRoot`. Отсутствующий маркер
/// уже считается успехом; он не влияет на работу установленного профиля.
pub(crate) fn finalize_migration(result: &MigrationResult) {
    let _ = fs::remove_file(result.destination.join(MIGRATION_MARKER));
}

fn directory_has_entries(path: &Path) -> Result<bool, String> {
    Ok(fs::read_dir(path)
        .map_err(|err| format!("Не удалось проверить новую папку: {err}"))?
        .next()
        .is_some())
}

fn retry_destination_matches(destination: &Path, source: &Path) -> Result<bool, String> {
    let marker = destination.join(MIGRATION_MARKER);
    if !marker.is_file() {
        return Ok(false);
    }
    let recorded = fs::read_to_string(&marker)
        .map_err(|err| format!("Не удалось прочитать маркер переноса: {err}"))?;
    if Path::new(recorded.trim()) != source {
        return Ok(false);
    }
    let entries = fs::read_dir(destination)
        .map_err(|err| format!("Не удалось проверить незавершённый перенос: {err}"))?;
    for entry in entries {
        let name = entry
            .map_err(|err| format!("Не удалось проверить незавершённый перенос: {err}"))?
            .file_name();
        if name != USERS_DIR
            && name != MIGRATION_MARKER
            && name != MIGRATION_DIR
            && name != MIGRATION_BACKUP_DIR
        {
            return Ok(false);
        }
    }
    Ok(true)
}

fn canonical_or_absolute(path: &Path) -> Result<PathBuf, String> {
    if path.exists() {
        return path
            .canonicalize()
            .map_err(|err| format!("Не удалось определить старую папку установки: {err}"));
    }
    if path.is_absolute() {
        return Ok(path.to_path_buf());
    }
    std::env::current_dir()
        .map(|cwd| cwd.join(path))
        .map_err(|err| format!("Не удалось определить старую папку установки: {err}"))
}

fn copy_tree(source: &Path, destination: &Path) -> Result<(), String> {
    fs::create_dir_all(destination)
        .map_err(|err| format!("Не удалось создать {}: {err}", destination.display()))?;

    let entries = fs::read_dir(source)
        .map_err(|err| format!("Не удалось прочитать {}: {err}", source.display()))?;
    for entry in entries {
        let entry = entry.map_err(|err| format!("Не удалось прочитать профиль: {err}"))?;
        let source_path = entry.path();
        let destination_path = destination.join(entry.file_name());
        let metadata = fs::symlink_metadata(&source_path)
            .map_err(|err| format!("Не удалось проверить {}: {err}", source_path.display()))?;

        if metadata.file_type().is_symlink() {
            return Err(format!(
                "Перенос остановлен: символические ссылки не поддерживаются ({})",
                source_path.display()
            ));
        }
        if metadata.is_dir() {
            copy_tree(&source_path, &destination_path)?;
            let _ = fs::set_permissions(&destination_path, metadata.permissions());
            sync_directory(&destination_path)?;
            continue;
        }
        if !metadata.is_file() {
            return Err(format!(
                "Перенос остановлен: неизвестный тип файла ({})",
                source_path.display()
            ));
        }

        let copied = fs::copy(&source_path, &destination_path)
            .map_err(|err| format!("Не удалось скопировать {}: {err}", source_path.display()))?;
        if copied != metadata.len() {
            return Err(format!(
                "Файл скопирован не полностью: {}",
                source_path.display()
            ));
        }
        if sha256_file(&source_path)? != sha256_file(&destination_path)? {
            return Err(format!(
                "Проверка скопированного файла не прошла: {}",
                source_path.display()
            ));
        }
        File::open(&destination_path)
            .and_then(|file| file.sync_all())
            .map_err(|err| {
                format!(
                    "Не удалось сбросить {} на диск: {err}",
                    destination_path.display()
                )
            })?;
    }
    sync_directory(destination)?;
    Ok(())
}

fn sync_directory(path: &Path) -> Result<(), String> {
    #[cfg(windows)]
    use std::os::windows::fs::OpenOptionsExt;

    #[cfg(windows)]
    let directory = fs::OpenOptions::new()
        .read(true)
        .write(true)
        .custom_flags(0x0200_0000) // FILE_FLAG_BACKUP_SEMANTICS
        .open(path);
    #[cfg(not(windows))]
    let directory = File::open(path);

    directory
        .and_then(|directory| directory.sync_all())
        .map_err(|err| {
            format!(
                "Не удалось сбросить каталог {} на диск: {err}",
                path.display()
            )
        })
}

fn sha256_file(path: &Path) -> Result<[u8; 32], String> {
    let mut file = File::open(path)
        .map_err(|err| format!("Не удалось проверить {}: {err}", path.display()))?;
    let mut hasher = Sha256::new();
    let mut buffer = [0_u8; 128 * 1024];
    loop {
        let read = file
            .read(&mut buffer)
            .map_err(|err| format!("Не удалось проверить {}: {err}", path.display()))?;
        if read == 0 {
            break;
        }
        hasher.update(&buffer[..read]);
    }
    Ok(hasher.finalize().into())
}

#[cfg(test)]
mod tests {
    use super::*;

    fn test_root(name: &str) -> PathBuf {
        std::env::temp_dir().join(format!("pjm_install_{name}_{}", std::process::id()))
    }

    #[test]
    fn old_settings_use_default_root() {
        let default = Path::new("/default/data");
        assert_eq!(configured_root(None, default).unwrap(), default);
        assert_eq!(configured_root(Some("  "), default).unwrap(), default);
    }

    #[test]
    fn migration_copies_profiles_and_keeps_source() {
        let root = test_root("copy");
        let source = root.join("source");
        let destination = root.join("destination");
        fs::create_dir_all(source.join("users/u/profiles/p/files/saves")).unwrap();
        fs::write(
            source.join("users/u/profiles/p/files/saves/world.dat"),
            b"world",
        )
        .unwrap();

        let result = migrate_users(&source, &destination).unwrap();
        finalize_migration(&result);

        assert!(result.copied_existing_data);
        assert_eq!(
            fs::read(destination.join("users/u/profiles/p/files/saves/world.dat")).unwrap(),
            b"world"
        );
        assert!(source
            .join("users/u/profiles/p/files/saves/world.dat")
            .exists());
        assert!(!destination.join(MIGRATION_DIR).exists());
        assert!(!destination.join(MIGRATION_MARKER).exists());
        let _ = fs::remove_dir_all(root);
    }

    #[test]
    fn migration_rejects_non_empty_destination_without_touching_it() {
        let root = test_root("nonempty");
        let source = root.join("source");
        let destination = root.join("destination");
        fs::create_dir_all(source.join("users")).unwrap();
        fs::create_dir_all(&destination).unwrap();
        fs::write(destination.join("keep.txt"), b"keep").unwrap();

        let error = migrate_users(&source, &destination).unwrap_err();

        assert!(error.contains("пустую папку"));
        assert_eq!(fs::read(destination.join("keep.txt")).unwrap(), b"keep");
        let _ = fs::remove_dir_all(root);
    }

    #[test]
    fn migration_can_retry_after_settings_write_failure() {
        let root = test_root("retry");
        let source = root.join("source");
        let destination = root.join("destination");
        fs::create_dir_all(source.join("users/u/profiles/p/files")).unwrap();
        fs::write(
            source.join("users/u/profiles/p/files/options.txt"),
            b"first",
        )
        .unwrap();

        let first = migrate_users(&source, &destination).unwrap();
        assert!(destination.join(MIGRATION_MARKER).exists());
        fs::write(
            source.join("users/u/profiles/p/files/options.txt"),
            b"second",
        )
        .unwrap();
        let retry = migrate_users(&source, &destination).unwrap();
        assert_eq!(
            fs::read(destination.join("users/u/profiles/p/files/options.txt")).unwrap(),
            b"second"
        );
        finalize_migration(&retry);
        assert!(!destination.join(MIGRATION_MARKER).exists());

        // `first` остаётся валидным результатом, но finalize уже идемпотентен.
        finalize_migration(&first);
        let _ = fs::remove_dir_all(root);
    }

    #[test]
    fn migration_recovers_partial_staging_after_process_interruption() {
        let root = test_root("interrupted");
        let source = root.join("source");
        let destination = root.join("destination");
        fs::create_dir_all(source.join("users/u/profiles/p/files")).unwrap();
        fs::write(
            source.join("users/u/profiles/p/files/options.txt"),
            b"complete",
        )
        .unwrap();
        fs::create_dir_all(destination.join(MIGRATION_DIR).join("users/u")).unwrap();
        let canonical_source = source.canonicalize().unwrap();
        fs::write(
            destination.join(MIGRATION_MARKER),
            canonical_source.to_string_lossy().as_bytes(),
        )
        .unwrap();
        fs::write(
            destination.join(MIGRATION_DIR).join("users/u/partial.txt"),
            b"partial",
        )
        .unwrap();

        let result = migrate_users(&source, &destination).unwrap();

        assert_eq!(
            fs::read(destination.join("users/u/profiles/p/files/options.txt")).unwrap(),
            b"complete"
        );
        assert!(!destination.join(MIGRATION_DIR).exists());
        finalize_migration(&result);
        let _ = fs::remove_dir_all(root);
    }
}
