fn main() {
    println!("cargo:rerun-if-env-changed=LAUNCHER_DEFAULT_API_URL");
    println!("cargo:rerun-if-env-changed=DISCORD_CLIENT_ID");
    // Публичный ключ подписи автообновления (option_env! в updater.rs): пересобрать
    // при смене, иначе во вшитом ключе останется старое/пустое значение.
    println!("cargo:rerun-if-env-changed=LAUNCHER_UPDATE_PUBKEY");

    tauri_build::build();
}
