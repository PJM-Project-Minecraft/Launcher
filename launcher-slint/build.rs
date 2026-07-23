fn main() {
    println!("cargo:rerun-if-env-changed=LAUNCHER_DEFAULT_API_URL");
    println!("cargo:rerun-if-env-changed=DISCORD_CLIENT_ID");
    // Публичный ключ подписи автообновления (option_env! в updater.rs): пересобрать
    // при смене, иначе во вшитом ключе останется старое/пустое значение.
    println!("cargo:rerun-if-env-changed=LAUNCHER_UPDATE_PUBKEY");

    // cfg(windows) в build.rs — это HOST, при кросс-сборке с Linux он ложен,
    // поэтому цель определяем через CARGO_CFG_TARGET_OS
    if std::env::var("CARGO_CFG_TARGET_OS").as_deref() == Ok("windows") {
        let mut res = winresource::WindowsResource::new();
        res.set_icon("assets/app.ico");
        if let Err(e) = res.compile() {
            println!("cargo:warning=failed to embed windows resource: {e}");
        }
    }

    slint_build::compile("ui/app.slint").expect("failed to compile Slint UI");
}
