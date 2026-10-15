// SPDX-License-Identifier: AGPL-3.0-or-later

use crate::{backup, navigation, process::DesktopState};
use tauri::{
    AppHandle,
    menu::{CheckMenuItem, MenuBuilder},
    tray::TrayIconBuilder,
};
use tauri_plugin_autostart::ManagerExt;
use tauri_plugin_opener::OpenerExt;

pub fn install(app: &AppHandle, state: DesktopState) -> tauri::Result<()> {
    let launch_at_login = app.autolaunch().is_enabled().unwrap_or(false);
    let login = CheckMenuItem::with_id(
        app,
        "launch-at-login",
        "Launch at Login",
        true,
        launch_at_login,
        None::<&str>,
    )?;
    let menu = MenuBuilder::new(app)
        .text("show-hide", "Show or Hide Suchi")
        .text("start", "Start Processing")
        .text("stop", "Stop Processing")
        .separator()
        .text("backup", "Create Complete Backup…")
        .text("restore", "Restore Complete Backup…")
        .separator()
        .text("open-data", "Open Data Folder")
        .text("open-backups", "Open Backup Folder")
        .text("open-logs", "Open Logs")
        .item(&login)
        .separator()
        .text("quit", "Quit Suchi")
        .build()?;
    let event_state = state.clone();
    let login_item = login.clone();
    let mut tray = TrayIconBuilder::new()
        .tooltip("Suchi")
        .menu(&menu)
        .show_menu_on_left_click(true)
        .on_menu_event(move |app, event| match event.id().as_ref() {
            "show-hide" => navigation::toggle_main_window(app),
            "start" => {
                let _ = event_state.start(app);
            }
            "stop" => event_state.stop(),
            "backup" => backup::choose_complete_backup(app, event_state.clone()),
            "restore" => backup::choose_complete_restore(app, event_state.clone()),
            "open-data" => {
                let _ = app
                    .opener()
                    .open_path(event_state.paths().archive.to_string_lossy(), None::<&str>);
            }
            "open-backups" => {
                let _ = app
                    .opener()
                    .open_path(event_state.paths().backups.to_string_lossy(), None::<&str>);
            }
            "open-logs" => {
                let _ = app
                    .opener()
                    .open_path(event_state.paths().logs.to_string_lossy(), None::<&str>);
            }
            "launch-at-login" => {
                let next = !app.autolaunch().is_enabled().unwrap_or(false);
                let result = if next {
                    app.autolaunch().enable()
                } else {
                    app.autolaunch().disable()
                };
                if result.is_ok() && event_state.set_launch_at_login_preference(next).is_ok() {
                    let _ = login_item.set_checked(next);
                }
            }
            "quit" => event_state.begin_quit(app.clone()),
            _ => {}
        });
    if let Some(icon) = app.default_window_icon() {
        tray = tray.icon(icon.clone());
    }
    tray.build(app)?;
    Ok(())
}
