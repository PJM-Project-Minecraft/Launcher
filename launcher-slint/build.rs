fn main() {
    println!("cargo:rerun-if-env-changed=LAUNCHER_DEFAULT_API_URL");
    println!("cargo:rerun-if-env-changed=DISCORD_CLIENT_ID");

    #[cfg(windows)]
    {
        let mut res = winresource::WindowsResource::new();
        res.set_icon("assets/app.ico");
        if let Err(e) = res.compile() {
            println!("cargo:warning=failed to embed windows resource: {e}");
        }
    }

    slint_build::compile("ui/app.slint").expect("failed to compile Slint UI");
}
