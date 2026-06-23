fn main() {
    println!("cargo:rerun-if-env-changed=LAUNCHER_DEFAULT_API_URL");
    println!("cargo:rerun-if-env-changed=DISCORD_CLIENT_ID");
    slint_build::compile("ui/app.slint").expect("failed to compile Slint UI");
}
