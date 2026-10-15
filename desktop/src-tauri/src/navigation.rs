// SPDX-License-Identifier: AGPL-3.0-or-later

use tauri::{AppHandle, Manager, Url};
use tauri_plugin_opener::OpenerExt;

pub const APPLICATION_ORIGIN: &str = "http://127.0.0.1:8000";

pub fn permits_navigation(app: &AppHandle, url: &Url) -> bool {
    if is_launcher_url(url) || is_application_url(url) {
        return true;
    }
    if matches!(url.scheme(), "https" | "mailto") {
        let _ = app.opener().open_url(url.as_str(), None::<&str>);
    }
    false
}

pub fn open_external(app: &AppHandle, url: &Url) {
    if matches!(url.scheme(), "https" | "mailto") {
        let _ = app.opener().open_url(url.as_str(), None::<&str>);
    }
}

pub fn navigate_to_application(app: &AppHandle) {
    navigate(app, APPLICATION_ORIGIN);
}

pub fn navigate_to_setup(app: &AppHandle) {
    navigate(app, &format!("{APPLICATION_ORIGIN}/bootstrap"));
}

pub fn navigate_to_launcher(app: &AppHandle) {
    #[cfg(target_os = "windows")]
    const URL: &str = "http://tauri.localhost/index.html";
    #[cfg(not(target_os = "windows"))]
    const URL: &str = "tauri://localhost/index.html";
    navigate(app, URL);
}

pub fn show_main_window(app: &AppHandle) {
    if let Some(window) = app.get_webview_window("main") {
        let _ = window.show();
        let _ = window.unminimize();
        let _ = window.set_focus();
    }
}

fn navigate(app: &AppHandle, target: &str) {
    if let Some(window) = app.get_webview_window("main")
        && let Ok(url) = Url::parse(target)
    {
        let _ = window.navigate(url);
        show_main_window(app);
    }
}

fn is_launcher_url(url: &Url) -> bool {
    (url.scheme() == "tauri" && url.host_str() == Some("localhost"))
        || (url.scheme() == "http" && url.host_str() == Some("tauri.localhost"))
}

fn is_application_url(url: &Url) -> bool {
    url.username().is_empty()
        && url.password().is_none()
        && url.origin().ascii_serialization() == APPLICATION_ORIGIN
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn application_origin_is_exact() {
        assert!(is_application_url(
            &Url::parse("http://127.0.0.1:8000/inbox").unwrap()
        ));
        assert!(!is_application_url(
            &Url::parse("http://localhost:8000/inbox").unwrap()
        ));
        assert!(!is_application_url(
            &Url::parse("http://127.0.0.1:8001/inbox").unwrap()
        ));
        assert!(!is_application_url(
            &Url::parse("https://127.0.0.1:8000/inbox").unwrap()
        ));
    }
}
