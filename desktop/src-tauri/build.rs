// SPDX-License-Identifier: AGPL-3.0-or-later

use std::{env, fs, path::PathBuf};

fn main() {
    let target = env::var("TARGET").expect("Cargo did not provide TARGET");
    println!("cargo:rustc-env=SUCHI_DESKTOP_TARGET={target}");

    let manifest = env::var_os("SUCHI_SIDECAR_IDENTITY")
        .map(PathBuf::from)
        .unwrap_or_else(|| PathBuf::from("generated/sidecar-identity.json"));
    println!("cargo:rerun-if-env-changed=SUCHI_SIDECAR_IDENTITY");
    println!("cargo:rerun-if-changed={}", manifest.display());
    let bytes = fs::read(&manifest).unwrap_or_else(|error| {
        panic!(
            "read sidecar identity {}: {error}; run `make desktop-sidecar`",
            manifest.display()
        )
    });
    let output = PathBuf::from(env::var_os("OUT_DIR").expect("Cargo did not provide OUT_DIR"))
        .join("sidecar-identity.json");
    fs::write(output, bytes).expect("write embedded sidecar identity");

    let attributes =
        tauri_build::Attributes::new().app_manifest(tauri_build::AppManifest::new().commands(&[
            "launcher_state",
            "start_processing",
            "stop_processing",
            "retry_processing",
            "reveal_setup_token",
            "open_setup",
            "open_application",
            "open_logs",
            "rollback_operation",
            "set_launch_at_login",
        ]));
    tauri_build::try_build(attributes).expect("build Tauri desktop metadata");
}
