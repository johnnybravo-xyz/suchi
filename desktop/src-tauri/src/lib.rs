// SPDX-License-Identifier: AGPL-3.0-or-later

mod commands;
mod environment;
mod identity;
mod logging;
mod navigation;
mod paths;
mod prerequisites;
mod process;
mod readiness;
mod tray;
mod window;

use identity::SidecarIdentity;
use paths::ManagedPaths;
use process::DesktopState;
use std::env;
use tauri::Manager;

pub fn run() {
    let initially_hidden = env::args_os().any(|argument| argument == "--hidden");
    let app = tauri::Builder::default()
        .plugin(tauri_plugin_single_instance::init(
            |app, _arguments, _cwd| {
                navigation::show_main_window(app);
            },
        ))
        .plugin(tauri_plugin_shell::init())
        .plugin(tauri_plugin_opener::init())
        .plugin(tauri_plugin_dialog::init())
        .plugin(
            tauri_plugin_autostart::Builder::new()
                .app_name("Suchi")
                .arg("--hidden")
                .build(),
        )
        .invoke_handler(tauri::generate_handler![
            commands::launcher_state,
            commands::start_processing,
            commands::stop_processing,
            commands::retry_processing,
            commands::reveal_setup_token,
            commands::open_setup,
            commands::open_application,
            commands::set_launch_at_login,
        ])
        .setup(move |app| {
            let handle = app.handle().clone();
            let paths = ManagedPaths::resolve(&handle).map_err(std::io::Error::other)?;
            let identity = SidecarIdentity::embedded().map_err(std::io::Error::other)?;
            let state =
                DesktopState::new(paths.clone(), identity).map_err(std::io::Error::other)?;
            app.manage(state.clone());
            window::create_main(&handle, &paths, initially_hidden)?;
            tray::install(&handle, state.clone())?;
            std::thread::spawn(move || {
                let _ = state.start(&handle);
            });
            Ok(())
        })
        .build(tauri::generate_context!())
        .expect("build Suchi desktop application");

    app.run(|app, event| match event {
        tauri::RunEvent::WindowEvent {
            label,
            event: tauri::WindowEvent::CloseRequested { api, .. },
            ..
        } if label == "main" => {
            let state = app.state::<DesktopState>();
            if !state.is_quitting() {
                api.prevent_close();
                if let Some(window) = app.get_webview_window("main") {
                    let _ = window.hide();
                }
            }
        }
        tauri::RunEvent::ExitRequested { api, .. } => {
            let state = app.state::<DesktopState>();
            if !state.is_quitting() {
                api.prevent_exit();
                state.begin_quit(app.clone());
            }
        }
        _ => {}
    });
}
