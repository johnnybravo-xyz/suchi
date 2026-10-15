// SPDX-License-Identifier: AGPL-3.0-or-later

use tauri::{AppHandle, Manager, Url};
use tauri_plugin_opener::OpenerExt;

pub fn permits_navigation(app: &AppHandle, url: &Url, application_origin: &str) -> bool {
    if is_launcher_url(url)
        || is_development_launcher_url(app, url)
        || is_application_url(url, application_origin)
    {
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

pub fn navigate_to_application(app: &AppHandle, application_origin: &str) {
    navigate(app, application_origin);
}

pub fn navigate_to_setup(app: &AppHandle, application_origin: &str) {
    navigate(app, &format!("{application_origin}/bootstrap"));
}

pub fn navigate_to_launcher(app: &AppHandle) {
    #[cfg(dev)]
    if let Some(url) = &app.config().build.dev_url {
        navigate(app, url.as_str());
        return;
    }

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

pub fn toggle_main_window(app: &AppHandle) {
    if let Some(window) = app.get_webview_window("main") {
        if window.is_visible().unwrap_or(false) {
            let _ = window.hide();
        } else {
            let _ = window.show();
            let _ = window.unminimize();
            let _ = window.set_focus();
        }
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

#[cfg(dev)]
fn is_development_launcher_url(app: &AppHandle, url: &Url) -> bool {
    app.config()
        .build
        .dev_url
        .as_ref()
        .is_some_and(|dev_url| url.origin() == dev_url.origin())
}

#[cfg(not(dev))]
fn is_development_launcher_url(_app: &AppHandle, _url: &Url) -> bool {
    false
}

fn is_application_url(url: &Url, application_origin: &str) -> bool {
    url.username().is_empty()
        && url.password().is_none()
        && url.origin().ascii_serialization() == application_origin
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn application_origin_is_exact() {
        assert!(is_application_url(
            &Url::parse("http://127.0.0.1:8123/inbox").unwrap(),
            "http://127.0.0.1:8123",
        ));
        assert!(!is_application_url(
            &Url::parse("http://localhost:8123/inbox").unwrap(),
            "http://127.0.0.1:8123",
        ));
        assert!(!is_application_url(
            &Url::parse("http://127.0.0.1:8001/inbox").unwrap(),
            "http://127.0.0.1:8123",
        ));
        assert!(!is_application_url(
            &Url::parse("https://127.0.0.1:8123/inbox").unwrap(),
            "http://127.0.0.1:8123",
        ));
    }
}
