// SPDX-License-Identifier: AGPL-3.0-or-later

use crate::paths::ManagedPaths;
use std::{collections::BTreeMap, ffi::OsString};

pub fn sidecar_environment(paths: &ManagedPaths) -> BTreeMap<OsString, OsString> {
    let mut environment = BTreeMap::new();
    for (key, value) in std::env::vars_os() {
        if allowed_host_variable(&key.to_string_lossy()) {
            environment.insert(key, value);
        }
    }

    set(&mut environment, "PATH", trusted_parser_path());
    set(&mut environment, "SUCHI_CONFIG", paths.config.as_os_str());
    set(&mut environment, "DATA_DIR", paths.archive.as_os_str());
    set(&mut environment, "DEPLOYMENT_PROFILE", "desktop_local");
    set(&mut environment, "PUBLIC_URL", "http://127.0.0.1:8000");
    set(&mut environment, "LISTEN_ADDR", "127.0.0.1:8000");
    set(&mut environment, "USE_LLM_CONNECTIONS", "off");
    set(&mut environment, "USE_EMAIL_INGESTION_FEATURES", "off");
    set(&mut environment, "SUCHI_DEV", "0");
    set(&mut environment, "SUCHI_DEMO_MODE", "0");
    set(&mut environment, "SUCHI_UI_DISABLED", "0");
    set(&mut environment, "SHARE_PUBLIC_URL", "");
    set(&mut environment, "SHARE_LISTEN_ADDR", "");
    set(&mut environment, "OIDC_ISSUER_URL", "");
    set(&mut environment, "OIDC_CLIENT_ID", "");
    set(&mut environment, "OIDC_CLIENT_SECRET", "");
    set(&mut environment, "TLS_CERT_FILE", "");
    set(&mut environment, "TLS_KEY_FILE", "");
    set(&mut environment, "LLM_ENDPOINT_URL", "");
    set(&mut environment, "LLM_MODEL", "");
    set(&mut environment, "LLM_API_KEY", "");
    set(&mut environment, "INGEST_IMAP_USER", "");
    set(&mut environment, "INGEST_FS_OWNER_EMAIL", "");
    environment
}

fn set<K, V>(environment: &mut BTreeMap<OsString, OsString>, key: K, value: V)
where
    K: Into<OsString>,
    V: Into<OsString>,
{
    environment.insert(key.into(), value.into());
}

fn allowed_host_variable(key: &str) -> bool {
    matches!(
        key,
        "HOME"
            | "USERPROFILE"
            | "TMPDIR"
            | "TEMP"
            | "TMP"
            | "LANG"
            | "SystemRoot"
            | "SYSTEMROOT"
            | "DISPLAY"
            | "WAYLAND_DISPLAY"
            | "XDG_RUNTIME_DIR"
            | "DBUS_SESSION_BUS_ADDRESS"
            | "__CF_USER_TEXT_ENCODING"
    ) || key.starts_with("LC_")
}

fn trusted_parser_path() -> OsString {
    #[cfg(target_os = "macos")]
    const VALUE: &str = "/opt/homebrew/bin:/usr/local/bin:/usr/bin:/bin:/usr/sbin:/sbin";
    #[cfg(target_os = "linux")]
    const VALUE: &str = "/usr/local/bin:/usr/bin:/bin";
    #[cfg(target_os = "windows")]
    let value = std::env::var_os("SystemRoot")
        .map(|root| format!(r"{}\System32", root.to_string_lossy()))
        .unwrap_or_else(|| r"C:\Windows\System32".to_owned());
    #[cfg(not(target_os = "windows"))]
    let value = VALUE.to_owned();
    value.into()
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn model_and_mail_settings_are_not_inherited() {
        assert!(!allowed_host_variable("LLM_API_KEY"));
        assert!(!allowed_host_variable("INGEST_IMAP_PASSWORD"));
        assert!(!allowed_host_variable("PATH"));
        assert!(allowed_host_variable("LANG"));
    }
}
