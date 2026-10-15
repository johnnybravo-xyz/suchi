// SPDX-License-Identifier: AGPL-3.0-or-later

use std::{
    fs::{self, File, OpenOptions},
    io::Write,
    path::{Path, PathBuf},
};
use tauri::{AppHandle, Manager};

#[derive(Clone, Debug)]
pub struct ManagedPaths {
    pub archive: PathBuf,
    pub backups: PathBuf,
    pub config: PathBuf,
    pub desktop_state: PathBuf,
    pub logs: PathBuf,
    pub runtime: PathBuf,
    pub webview_cache: PathBuf,
}

impl ManagedPaths {
    pub fn resolve(app: &AppHandle) -> Result<Self, String> {
        let data_root = app
            .path()
            .app_data_dir()
            .map_err(|_| "The application data directory is unavailable.".to_owned())?;
        let config_root = app
            .path()
            .app_config_dir()
            .map_err(|_| "The application configuration directory is unavailable.".to_owned())?;
        let cache_root = app
            .path()
            .app_cache_dir()
            .map_err(|_| "The application cache directory is unavailable.".to_owned())?;
        let logs = app
            .path()
            .app_log_dir()
            .map_err(|_| "The application log directory is unavailable.".to_owned())?;

        let paths = Self {
            archive: data_root.join("archive"),
            backups: data_root.join("backups"),
            config: config_root.join("local-install.toml"),
            desktop_state: data_root.join("desktop-state.json"),
            logs,
            runtime: cache_root.join("runtime"),
            webview_cache: cache_root.join("webview"),
        };
        for directory in [
            &paths.archive,
            &paths.backups,
            paths.config.parent().expect("config has parent"),
            &paths.logs,
            &paths.runtime,
            &paths.webview_cache,
        ] {
            create_private_directory(directory)?;
        }
        Ok(paths)
    }

    pub fn with_legacy_archive(
        &self,
        data_root: PathBuf,
        archive: PathBuf,
        config: PathBuf,
    ) -> Result<Self, String> {
        let backups = data_root.join("backups");
        create_private_directory(&backups)?;
        Ok(Self {
            archive,
            backups,
            config,
            desktop_state: self.desktop_state.clone(),
            logs: self.logs.clone(),
            runtime: self.runtime.clone(),
            webview_cache: self.webview_cache.clone(),
        })
    }

    pub fn ready_file(&self, nonce: &str) -> PathBuf {
        self.runtime.join(format!("ready-{nonce}.json"))
    }

    pub fn setup_token_file(&self) -> PathBuf {
        self.runtime.join("setup-token")
    }

    pub fn sidecar_log(&self) -> PathBuf {
        self.logs.join("sidecar.log")
    }

    pub fn write_managed_config(&self, port: u16) -> Result<(), String> {
        reject_unsafe_existing(&self.config)?;
        let path = toml_string(&self.archive);
        let origin = format!("http://127.0.0.1:{port}");
        let listen = format!("127.0.0.1:{port}");
        let contents = format!(
            "# Managed by Suchi Desktop. Manual changes are replaced at launch.\n\
             deployment_profile = \"desktop_local\"\n\
             public_url = {origin:?}\n\
             listen_addr = {listen:?}\n\
             data_dir = {path}\n\
             use_llm_connections = \"off\"\n\
             use_email_ingestion_features = \"off\"\n\
             share_public_url = \"\"\n\
             share_listen_addr = \"\"\n\
             oidc_issuer_url = \"\"\n\
             oidc_client_id = \"\"\n\
             tls_cert_file = \"\"\n\
             tls_key_file = \"\"\n\
             suchi_dev = false\n\
             suchi_demo_mode = false\n\
             suchi_ui_disabled = false\n\
             ocr_languages = [\"eng\"]\n"
        );
        atomic_write_private(&self.config, contents.as_bytes())
    }
}

fn toml_string(path: &Path) -> String {
    serde_json::to_string(&path.to_string_lossy()).expect("serialize path string")
}

fn reject_unsafe_existing(path: &Path) -> Result<(), String> {
    let metadata = match fs::symlink_metadata(path) {
        Ok(metadata) => metadata,
        Err(error) if error.kind() == std::io::ErrorKind::NotFound => return Ok(()),
        Err(_) => return Err("The managed configuration cannot be inspected.".to_owned()),
    };
    if metadata.file_type().is_symlink() || !metadata.is_file() {
        return Err("The managed configuration has an unsafe file type.".to_owned());
    }
    Ok(())
}

pub fn create_private_directory(path: &Path) -> Result<(), String> {
    fs::create_dir_all(path)
        .map_err(|_| format!("Cannot create the private directory {}.", path.display()))?;
    let metadata = fs::symlink_metadata(path)
        .map_err(|_| format!("Cannot inspect the private directory {}.", path.display()))?;
    if metadata.file_type().is_symlink() || !metadata.is_dir() {
        return Err(format!("The private path {} is unsafe.", path.display()));
    }
    set_private_directory_permissions(path)?;
    Ok(())
}

pub(crate) fn atomic_write_private(path: &Path, contents: &[u8]) -> Result<(), String> {
    let parent = path
        .parent()
        .ok_or_else(|| "The managed file has no parent directory.".to_owned())?;
    let temporary = parent.join(format!(
        ".{}.{}.tmp",
        path.file_name()
            .and_then(|name| name.to_str())
            .unwrap_or("managed"),
        std::process::id()
    ));
    let _ = fs::remove_file(&temporary);
    let mut options = OpenOptions::new();
    options.write(true).create_new(true);
    #[cfg(unix)]
    {
        use std::os::unix::fs::OpenOptionsExt;
        options.mode(0o600);
    }
    let mut file = options
        .open(&temporary)
        .map_err(|_| "Cannot create the private managed file.".to_owned())?;
    file.write_all(contents)
        .and_then(|()| file.sync_all())
        .map_err(|_| "Cannot write the private managed file.".to_owned())?;
    fs::rename(&temporary, path)
        .map_err(|_| "Cannot activate the private managed file.".to_owned())?;
    sync_parent(parent)?;
    Ok(())
}

#[cfg(unix)]
fn sync_parent(parent: &Path) -> Result<(), String> {
    File::open(parent)
        .and_then(|directory| directory.sync_all())
        .map_err(|_| "Cannot sync the private managed file directory.".to_owned())
}

#[cfg(not(unix))]
fn sync_parent(_parent: &Path) -> Result<(), String> {
    Ok(())
}

#[cfg(unix)]
fn set_private_directory_permissions(path: &Path) -> Result<(), String> {
    use std::os::unix::fs::PermissionsExt;
    fs::set_permissions(path, fs::Permissions::from_mode(0o700))
        .map_err(|_| format!("Cannot protect the private directory {}.", path.display()))
}

#[cfg(not(unix))]
fn set_private_directory_permissions(_path: &Path) -> Result<(), String> {
    Ok(())
}
