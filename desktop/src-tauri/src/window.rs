// SPDX-License-Identifier: AGPL-3.0-or-later

use crate::{navigation, paths::ManagedPaths};
use tauri::{
    AppHandle, WebviewUrl, WebviewWindow, WebviewWindowBuilder, webview::NewWindowResponse,
};

pub fn create_main(
    app: &AppHandle,
    paths: &ManagedPaths,
    initially_hidden: bool,
) -> tauri::Result<WebviewWindow> {
    let navigation_app = app.clone();
    let new_window_app = app.clone();
    WebviewWindowBuilder::new(app, "main", WebviewUrl::App("index.html".into()))
        .title("Suchi")
        .inner_size(1100.0, 760.0)
        .min_inner_size(760.0, 560.0)
        .center()
        .visible(!initially_hidden)
        .data_directory(paths.webview_cache.clone())
        .disable_drag_drop_handler()
        .devtools(cfg!(debug_assertions))
        .on_navigation(move |url| navigation::permits_navigation(&navigation_app, url))
        .on_new_window(move |url, _features| {
            navigation::open_external(&new_window_app, &url);
            NewWindowResponse::Deny
        })
        .build()
}
