// SPDX-License-Identifier: AGPL-3.0-or-later

mod backup;
mod commands;
mod desktop_state;
mod environment;
mod identity;
mod legacy;
mod logging;
mod navigation;
mod paths;
mod prerequisites;
mod process;
mod readiness;
mod sidecar_cli;
mod tray;
mod upgrade;
mod window;

use desktop_state::DesktopStateStore;
use identity::SidecarIdentity;
use paths::ManagedPaths;
use process::DesktopState;
use std::env;
use tauri::Manager;
use tauri_plugin_autostart::ManagerExt as _;

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
            commands::rollback_operation,
            commands::set_launch_at_login,
        ])
        .setup(move |app| {
            let handle = app.handle().clone();
            let modern_paths = ManagedPaths::resolve(&handle).map_err(std::io::Error::other)?;
            let desktop_state = DesktopStateStore::load(modern_paths.desktop_state.clone())
                .map_err(std::io::Error::other)?;
            let (paths, legacy_install, startup_blocker) =
                match legacy::detect(&handle, &modern_paths, &desktop_state.snapshot()) {
                    Ok(Some((paths, install))) => {
                        desktop_state
                            .update(|state| {
                                state.selected_port = install.port();
                                state.launch_at_login = install.launch_at_login();
                                Ok(())
                            })
                            .map_err(std::io::Error::other)?;
                        (paths, Some(install), None)
                    }
                    Ok(None) => (modern_paths, None, None),
                    Err(message) => (modern_paths, None, Some(message)),
                };
            desktop_state.persist().map_err(std::io::Error::other)?;
            let identity = SidecarIdentity::embedded().map_err(std::io::Error::other)?;
            let preserve_legacy_preference = legacy_install.is_some();
            let state = DesktopState::new(
                paths.clone(),
                identity,
                desktop_state,
                legacy_install,
                startup_blocker.clone(),
            )
            .map_err(std::io::Error::other)?;
            if startup_blocker.is_none() && !preserve_legacy_preference {
                state
                    .set_launch_at_login_preference(
                        handle.autolaunch().is_enabled().unwrap_or(false),
                    )
                    .map_err(std::io::Error::other)?;
            }
            let origin = state.origin().to_owned();
            app.manage(state.clone());
            window::create_main(&handle, &paths, origin, initially_hidden)?;
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
