// SPDX-License-Identifier: AGPL-3.0-or-later

use crate::process::DesktopState;
use tauri::{
    AppHandle, Manager,
    menu::{AboutMetadata, MenuBuilder, MenuItem, PredefinedMenuItem, SubmenuBuilder},
};
use tauri_plugin_opener::OpenerExt;

const OPEN_LOGS: &str = "app-open-logs";
const RELOAD: &str = "app-reload";
const QUIT: &str = "app-quit";

pub fn install(app: &AppHandle, state: DesktopState) -> tauri::Result<()> {
    let package = app.package_info();
    let build = state.build_identity();
    let about = PredefinedMenuItem::about(
        app,
        Some("About Suchi"),
        Some(AboutMetadata {
            name: Some("Suchi".to_owned()),
            version: Some(package.version.to_string()),
            comments: Some(format!(
                "Private local document archive\nBundled server {} ({})",
                build.version, build.revision
            )),
            copyright: app.config().bundle.copyright.clone(),
            license: Some("AGPL-3.0-or-later or commercial license".to_owned()),
            website: Some("https://suchi.page".to_owned()),
            website_label: Some("suchi.page".to_owned()),
            icon: app.default_window_icon().cloned(),
            ..Default::default()
        }),
    )?;
    let open_logs = MenuItem::with_id(app, OPEN_LOGS, "Open Logs", true, None::<&str>)?;
    let reload = MenuItem::with_id(app, RELOAD, "Reload", true, Some("CmdOrCtrl+R"))?;
    let quit = MenuItem::with_id(app, QUIT, "Quit Suchi", true, Some("CmdOrCtrl+Q"))?;

    let suchi = SubmenuBuilder::new(app, "Suchi")
        .item(&about)
        .separator()
        .item(&open_logs)
        .separator()
        .item(&quit)
        .build()?;
    let view = SubmenuBuilder::new(app, "View").item(&reload).build()?;
    let menu = MenuBuilder::new(app).items(&[&suchi, &view]).build()?;
    app.set_menu(menu)?;

    app.on_menu_event(move |app, event| match event.id().as_ref() {
        OPEN_LOGS => {
            let _ = app
                .opener()
                .open_path(state.paths().logs.to_string_lossy(), None::<&str>);
        }
        RELOAD => {
            if let Some(window) = app.get_webview_window("main") {
                let _ = window.reload();
            }
        }
        QUIT => state.begin_quit(app.clone()),
        _ => {}
    });
    Ok(())
}
