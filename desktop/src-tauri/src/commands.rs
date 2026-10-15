// SPDX-License-Identifier: AGPL-3.0-or-later

use crate::{
    backup, navigation,
    process::{DesktopState, LauncherState},
};
use tauri::{AppHandle, State};
use tauri_plugin_autostart::ManagerExt;

#[tauri::command]
pub fn launcher_state(state: State<'_, DesktopState>) -> LauncherState {
    state.launcher_state()
}

#[tauri::command]
pub fn start_processing(app: AppHandle, state: State<'_, DesktopState>) -> Result<(), String> {
    state.start(&app)
}

#[tauri::command]
pub fn stop_processing(state: State<'_, DesktopState>) {
    state.stop();
}

#[tauri::command]
pub fn retry_processing(app: AppHandle, state: State<'_, DesktopState>) -> Result<(), String> {
    state.start(&app)
}

#[tauri::command]
pub fn reveal_setup_token(state: State<'_, DesktopState>) -> Result<String, String> {
    state.reveal_setup_token()
}

#[tauri::command]
pub fn open_setup(app: AppHandle, state: State<'_, DesktopState>) {
    navigation::navigate_to_setup(&app, state.origin());
}

#[tauri::command]
pub fn open_application(app: AppHandle, state: State<'_, DesktopState>) {
    navigation::navigate_to_application(&app, state.origin());
}

#[tauri::command]
pub fn rollback_operation(app: AppHandle, state: State<'_, DesktopState>) {
    backup::rollback_pending(app, state.inner().clone());
}

#[tauri::command]
pub fn set_launch_at_login(
    app: AppHandle,
    state: State<'_, DesktopState>,
    enabled: bool,
) -> Result<(), String> {
    let manager = app.autolaunch();
    if enabled {
        manager.enable()
    } else {
        manager.disable()
    }
    .map_err(|_| "The launch-at-login setting could not be changed.".to_owned())?;
    state.set_launch_at_login_preference(enabled)
}
