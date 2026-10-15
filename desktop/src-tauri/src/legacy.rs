// SPDX-License-Identifier: AGPL-3.0-or-later

use crate::{
    backup,
    desktop_state::{BuildIdentity, DesktopStateFile, OperationKind, PendingOperation},
    paths::ManagedPaths,
    process::DesktopState,
};
#[cfg(target_os = "linux")]
use std::{env, process::Output};
use std::{
    ffi::OsStr,
    fs,
    net::TcpListener,
    path::{Path, PathBuf},
    process::Command,
    thread,
    time::{Duration, Instant},
};
use tauri::{AppHandle, Manager};
use tauri_plugin_dialog::DialogExt;

#[cfg(all(test, target_os = "macos"))]
#[allow(dead_code)]
#[path = "../tests/support/legacy_fixtures.rs"]
mod legacy_fixtures;

const MANAGED_MARKER: &[u8] = b"Managed by Suchi local installer.\n";
const PORT_WAIT: Duration = Duration::from_secs(65);

#[derive(Clone, Debug)]
pub struct LegacyInstall {
    layout: LegacyLayout,
    port: u16,
    launch_at_login: bool,
}

#[derive(Clone, Debug)]
enum LegacyLayout {
    #[cfg(target_os = "linux")]
    Linux(LinuxLayout),
    #[cfg(target_os = "macos")]
    Mac(MacLayout),
}

#[cfg(target_os = "linux")]
#[derive(Clone, Debug)]
struct LinuxLayout {
    home: PathBuf,
    data_base: PathBuf,
    config_base: PathBuf,
    root: PathBuf,
    archive: PathBuf,
    config: PathBuf,
    program: PathBuf,
    marker: PathBuf,
    unit: PathBuf,
    desktop_entry: PathBuf,
}

#[cfg(target_os = "macos")]
#[derive(Clone, Debug)]
struct MacLayout {
    home: PathBuf,
    root: PathBuf,
    archive: PathBuf,
    config: PathBuf,
    runtime: PathBuf,
    program: PathBuf,
    marker: PathBuf,
    service_plist: PathBuf,
    login_plist: PathBuf,
    app: PathBuf,
    settings_app: PathBuf,
}

pub fn detect(
    app: &AppHandle,
    modern: &ManagedPaths,
    state: &DesktopStateFile,
) -> Result<Option<(ManagedPaths, LegacyInstall)>, String> {
    let resume = state.legacy_migration_complete
        || state
            .pending_operation
            .as_ref()
            .is_some_and(|pending| pending.kind == OperationKind::LegacyMigration)
        || state
            .last_ready_build
            .as_ref()
            .is_some_and(|build| build.version == "legacy-local-installer");

    #[cfg(target_os = "linux")]
    let found = detect_linux(app, state, resume)?;
    #[cfg(target_os = "macos")]
    let found = detect_macos(app, state, resume)?;
    #[cfg(not(any(target_os = "linux", target_os = "macos")))]
    let found: Option<LegacyInstall> = None;

    let Some(install) = found else {
        return Ok(None);
    };
    let (root, archive, config) = install.archive_paths();
    let paths = modern.with_legacy_archive(root, archive, config)?;
    Ok(Some((paths, install)))
}

pub fn prepare(
    app: &AppHandle,
    state: &DesktopState,
    executable: &Path,
    install: &LegacyInstall,
) -> Result<(), String> {
    if state.state_store().snapshot().legacy_migration_complete {
        return Ok(());
    }
    install.stop_and_disable()?;
    wait_for_free_port(install.port)?;

    let snapshot = state.state_store().snapshot();
    match snapshot.pending_operation {
        Some(pending) if pending.kind == OperationKind::LegacyMigration => {
            if pending.target_build != state.build_identity() {
                return Err(
                    "The interrupted legacy migration belongs to a different installed build. Reinstall that build to retry or restore its retained backup."
                        .to_owned(),
                );
            }
        }
        Some(_) => {
            return Err(
                "Another archive operation is pending. Finish or roll it back before migrating the legacy installation."
                    .to_owned(),
            );
        }
        None => {
            let _guard = state.begin_operation()?;
            let destination = backup::automatic_destination(state, "before-legacy-migration");
            let verified = match backup::create_verified(state, executable, &destination, false) {
                Ok(verified) => verified,
                Err(message)
                    if message.starts_with("The backup destination needs approximately") =>
                {
                    let selected = app
                        .dialog()
                        .file()
                        .set_title("Choose External Legacy Migration Backup Destination")
                        .set_file_name("suchi-before-legacy-migration.suchi-backup")
                        .add_filter("Suchi complete backup", &["suchi-backup"])
                        .blocking_save_file()
                        .ok_or_else(|| {
                            "Legacy migration requires a verified complete backup. Choose an external destination with enough free space, then retry."
                                .to_owned()
                        })?
                        .into_path()
                        .map_err(|_| {
                            "The selected external backup destination is invalid.".to_owned()
                        })?;
                    backup::create_verified(state, executable, &selected, true)?
                }
                Err(message) => return Err(message),
            };
            let source = BuildIdentity {
                version: "legacy-local-installer".to_owned(),
                revision: "unrecorded".to_owned(),
            };
            let target = state.build_identity();
            state.state_store().update(|desktop| {
                desktop.pending_operation = Some(PendingOperation {
                    kind: OperationKind::LegacyMigration,
                    source_build: source,
                    target_build: target,
                    backup_path: verified.path,
                    backup_sha256: verified.sha256,
                    restore_source: None,
                    restore_id: None,
                });
                Ok(())
            })?;
        }
    }

    install.remove_owned_integration()?;
    wait_for_free_port(install.port)
}

impl LegacyInstall {
    pub fn port(&self) -> u16 {
        self.port
    }

    pub fn launch_at_login(&self) -> bool {
        self.launch_at_login
    }

    fn archive_paths(&self) -> (PathBuf, PathBuf, PathBuf) {
        match &self.layout {
            #[cfg(target_os = "linux")]
            LegacyLayout::Linux(layout) => (
                layout.root.clone(),
                layout.archive.clone(),
                layout.config.clone(),
            ),
            #[cfg(target_os = "macos")]
            LegacyLayout::Mac(layout) => (
                layout.root.clone(),
                layout.archive.clone(),
                layout.config.clone(),
            ),
        }
    }

    fn stop_and_disable(&self) -> Result<(), String> {
        match &self.layout {
            #[cfg(target_os = "linux")]
            LegacyLayout::Linux(layout) => stop_linux(layout),
            #[cfg(target_os = "macos")]
            LegacyLayout::Mac(layout) => stop_macos(layout, self.launch_at_login),
        }
    }

    fn remove_owned_integration(&self) -> Result<(), String> {
        match &self.layout {
            #[cfg(target_os = "linux")]
            LegacyLayout::Linux(layout) => remove_linux(layout, self.port),
            #[cfg(target_os = "macos")]
            LegacyLayout::Mac(layout) => remove_macos(layout, self.port, self.launch_at_login),
        }
    }
}

#[cfg(target_os = "linux")]
fn detect_linux(
    app: &AppHandle,
    state: &DesktopStateFile,
    resume: bool,
) -> Result<Option<LegacyInstall>, String> {
    let home = app
        .path()
        .home_dir()
        .map_err(|_| "The home directory is unavailable for legacy migration.".to_owned())?;
    let data_base = env::var_os("XDG_DATA_HOME")
        .map(PathBuf::from)
        .unwrap_or_else(|| home.join(".local/share"));
    let config_base = env::var_os("XDG_CONFIG_HOME")
        .map(PathBuf::from)
        .unwrap_or_else(|| home.join(".config"));
    let root = data_base.join("suchi-local");
    let program = root.join("program");
    let layout = LinuxLayout {
        home: home.clone(),
        data_base: data_base.clone(),
        config_base: config_base.clone(),
        archive: root.join("archive"),
        config: config_base.join("suchi-local/local-install.toml"),
        marker: program.join("managed-by-suchi-local"),
        unit: config_base.join("systemd/user/suchi-local.service"),
        desktop_entry: data_base.join("applications/suchi-local.desktop"),
        program,
        root,
    };
    if !resume && !path_exists_without_following(&layout.marker)? {
        return Ok(None);
    }
    let (port, launch_at_login) = if resume {
        validate_linux_resume(&layout, state.selected_port)?;
        (state.selected_port, state.launch_at_login)
    } else {
        validate_linux_files(&layout)?
    };
    Ok(Some(LegacyInstall {
        layout: LegacyLayout::Linux(layout),
        port,
        launch_at_login,
    }))
}

#[cfg(target_os = "linux")]
fn validate_linux_files(layout: &LinuxLayout) -> Result<(u16, bool), String> {
    for (path, label) in [
        (layout.home.as_path(), "home directory"),
        (&layout.data_base, "legacy data base directory"),
        (&layout.config_base, "legacy configuration base directory"),
        (&layout.root, "legacy data directory"),
        (&layout.archive, "legacy archive"),
        (
            layout.config.parent().expect("legacy config parent"),
            "legacy configuration directory",
        ),
        (
            layout.unit.parent().expect("legacy unit parent"),
            "legacy systemd directory",
        ),
        (
            layout
                .desktop_entry
                .parent()
                .expect("legacy desktop parent"),
            "legacy applications directory",
        ),
        (&layout.program, "legacy program directory"),
    ] {
        require_owned_directory(path, label)?;
    }
    require_owned_regular(&layout.config, "legacy configuration")?;
    require_exact(&layout.marker, MANAGED_MARKER, "legacy ownership marker")?;
    let allowed = [
        "suchi",
        "install.sh",
        "launch.sh",
        "suchi-tile.svg",
        "port",
        "open-with",
        "managed-by-suchi-local",
    ];
    require_exact_children(&layout.program, &allowed)?;
    for name in allowed {
        require_owned_regular(&layout.program.join(name), "legacy program file")?;
    }
    let port = read_port(&layout.program.join("port"))?;
    require_exact(
        &layout.unit,
        linux_unit_contents(layout, &layout.home, port).as_bytes(),
        "legacy systemd service",
    )?;
    require_exact(
        &layout.desktop_entry,
        linux_desktop_contents(layout).as_bytes(),
        "legacy desktop entry",
    )?;
    validate_linux_service(layout)?;
    let output = systemctl(["--user", "is-enabled", "suchi-local.service"])?;
    let status = String::from_utf8_lossy(&output.stdout).trim().to_owned();
    let launch_at_login = if output.status.success() && status == "enabled" {
        true
    } else if !output.status.success() && status == "disabled" {
        false
    } else {
        return Err(
            "The legacy systemd login state is not exactly enabled or disabled. Run `systemctl --user disable suchi-local.service`, then retry."
                .to_owned(),
        );
    };
    Ok((port, launch_at_login))
}

#[cfg(target_os = "linux")]
fn validate_linux_resume(layout: &LinuxLayout, port: u16) -> Result<(), String> {
    for (path, label) in [
        (layout.home.as_path(), "home directory"),
        (&layout.data_base, "legacy data base directory"),
        (&layout.config_base, "legacy configuration base directory"),
        (&layout.root, "legacy data directory"),
        (&layout.archive, "legacy archive"),
        (
            layout.config.parent().expect("legacy config parent"),
            "legacy configuration directory",
        ),
    ] {
        require_owned_directory(path, label)?;
    }
    require_owned_regular(&layout.config, "legacy configuration")?;
    if path_exists_without_following(&layout.program)? {
        require_owned_directory(&layout.program, "legacy program directory")?;
        let allowed = [
            "suchi",
            "install.sh",
            "launch.sh",
            "suchi-tile.svg",
            "port",
            "open-with",
            "managed-by-suchi-local",
        ];
        reject_unknown_children(&layout.program, &allowed)?;
        if path_exists_without_following(&layout.marker)? {
            require_exact(&layout.marker, MANAGED_MARKER, "legacy ownership marker")?;
        }
        if path_exists_without_following(&layout.program.join("port"))?
            && read_port(&layout.program.join("port"))? != port
        {
            return Err("The legacy port changed during migration. Restore the retained backup or restore the original port file, then retry.".to_owned());
        }
    }
    Ok(())
}

#[cfg(target_os = "linux")]
fn validate_linux_service(layout: &LinuxLayout) -> Result<(), String> {
    let output = systemctl([
        "--user",
        "show",
        "--property=LoadState",
        "--property=FragmentPath",
        "--property=DropInPaths",
        "suchi-local.service",
    ])?;
    if !output.status.success() {
        return Err("The legacy systemd service could not be inspected. Run inside the desktop login session, then retry.".to_owned());
    }
    let report = String::from_utf8_lossy(&output.stdout);
    let mut load_state = "";
    let mut fragment = "";
    let mut drop_ins = "";
    for line in report.lines() {
        if let Some(value) = line.strip_prefix("LoadState=") {
            load_state = value;
        } else if let Some(value) = line.strip_prefix("FragmentPath=") {
            fragment = value;
        } else if let Some(value) = line.strip_prefix("DropInPaths=") {
            drop_ins = value;
        }
    }
    if load_state != "loaded" || fragment != layout.unit.to_string_lossy() || !drop_ins.is_empty() {
        return Err("The legacy suchi-local.service has a foreign fragment or drop-in. Remove the conflicting service definition without deleting the archive, then retry.".to_owned());
    }
    Ok(())
}

#[cfg(target_os = "linux")]
fn stop_linux(layout: &LinuxLayout) -> Result<(), String> {
    if path_exists_without_following(&layout.unit)? {
        validate_linux_service(layout)?;
        let output = systemctl(["--user", "disable", "--now", "suchi-local.service"])?;
        if !output.status.success() {
            return Err("The legacy systemd service could not be stopped and disabled. Run `systemctl --user disable --now suchi-local.service`, wait for it to exit, then retry.".to_owned());
        }
    }
    Ok(())
}

#[cfg(target_os = "linux")]
fn remove_linux(layout: &LinuxLayout, port: u16) -> Result<(), String> {
    if path_exists_without_following(&layout.unit)? {
        require_exact(
            &layout.unit,
            linux_unit_contents(layout, &layout.home, port).as_bytes(),
            "legacy systemd service",
        )?;
        remove_owned_file(&layout.unit)?;
    }
    if path_exists_without_following(&layout.desktop_entry)? {
        require_exact(
            &layout.desktop_entry,
            linux_desktop_contents(layout).as_bytes(),
            "legacy desktop entry",
        )?;
        remove_owned_file(&layout.desktop_entry)?;
    }
    if path_exists_without_following(&layout.program)? {
        let allowed = [
            "suchi",
            "install.sh",
            "launch.sh",
            "suchi-tile.svg",
            "port",
            "open-with",
            "managed-by-suchi-local",
        ];
        reject_unknown_children(&layout.program, &allowed)?;
        for name in allowed {
            remove_owned_file_if_present(&layout.program.join(name))?;
        }
        fs::remove_dir(&layout.program).map_err(|_| {
            format!(
                "The legacy program directory could not be removed: {}. Remove only its installer-owned files, then retry.",
                layout.program.display()
            )
        })?;
    }
    let output = systemctl(["--user", "daemon-reload"])?;
    if !output.status.success() {
        return Err("systemd could not reload after legacy cleanup. Run `systemctl --user daemon-reload`, then retry.".to_owned());
    }
    Ok(())
}

#[cfg(target_os = "linux")]
fn systemctl<const N: usize>(arguments: [&str; N]) -> Result<Output, String> {
    Command::new("/usr/bin/systemctl")
        .args(arguments)
        .output()
        .map_err(|_| "The systemd user manager could not be invoked.".to_owned())
}

#[cfg(target_os = "linux")]
fn linux_unit_contents(layout: &LinuxLayout, home: &Path, port: u16) -> String {
    format!(
        "# Managed by Suchi local installer.\n[Unit]\nDescription=Suchi local document archive\n\n[Service]\nType=notify\nNotifyAccess=main\nRuntimeDirectory=suchi-local\nRuntimeDirectoryMode=0700\nUMask=0077\nWorkingDirectory={}\nExecStart=/usr/bin/env -i \"HOME={}\" \"PATH=/usr/local/bin:/usr/bin:/bin\" \"NOTIFY_SOCKET=${{NOTIFY_SOCKET}}\" \"DATA_DIR={}\" \"SUCHI_CONFIG={}\" \"LISTEN_ADDR=127.0.0.1:{port}\" \"PUBLIC_URL=http://suchi.localhost:{port}\" \"SUCHI_DEV=0\" \"SUCHI_DEMO_MODE=0\" \"{}/suchi\" serve --systemd \"--setup-token-file=%t/suchi-local/setup-token\"\nRestart=on-failure\nRestartSec=5s\nTimeoutStartSec=120s\nTimeoutStopSec=60s\nNoNewPrivileges=true\n\n[Install]\nWantedBy=default.target\n",
        layout.archive.display(),
        home.display(),
        layout.archive.display(),
        layout.config.display(),
        layout.program.display(),
    )
}

#[cfg(target_os = "linux")]
fn linux_desktop_contents(layout: &LinuxLayout) -> String {
    format!(
        "# Managed by Suchi local installer.\n[Desktop Entry]\nType=Application\nName=Suchi\nComment=Open your local document archive\nExec=\"{0}/launch.sh\"\nIcon={0}/suchi-tile.svg\nTerminal=false\nCategories=Office;\nActions=Settings;Stop;\n\n[Desktop Action Settings]\nName=Settings\nExec=\"{0}/launch.sh\" --settings\n\n[Desktop Action Stop]\nName=Stop Suchi\nExec=\"{0}/launch.sh\" --stop\n",
        layout.program.display(),
    )
}

#[cfg(target_os = "macos")]
fn detect_macos(
    app: &AppHandle,
    state: &DesktopStateFile,
    resume: bool,
) -> Result<Option<LegacyInstall>, String> {
    let home = app
        .path()
        .home_dir()
        .map_err(|_| "The home directory is unavailable for legacy migration.".to_owned())?;
    let root = home.join("Library/Application Support/Suchi");
    let program = root.join("program");
    let layout = MacLayout {
        archive: root.join("archive"),
        config: root.join("config/local-install.toml"),
        runtime: root.join("runtime"),
        marker: program.join("managed-by-suchi-local"),
        service_plist: program.join("page.suchi.local.plist"),
        login_plist: home.join("Library/LaunchAgents/page.suchi.local.plist"),
        app: home.join("Applications/Suchi.app"),
        settings_app: home.join("Applications/Suchi Settings.app"),
        program,
        root,
        home,
    };
    if !resume && !path_exists_without_following(&layout.marker)? {
        return Ok(None);
    }
    let (port, launch_at_login) = if resume {
        validate_macos_resume(&layout, state.selected_port)?;
        (state.selected_port, state.launch_at_login)
    } else {
        validate_macos_files(&layout)?
    };
    Ok(Some(LegacyInstall {
        layout: LegacyLayout::Mac(layout),
        port,
        launch_at_login,
    }))
}

#[cfg(target_os = "macos")]
fn validate_macos_artifacts(layout: &MacLayout) -> Result<(u16, bool), String> {
    for (path, label) in [
        (layout.home.as_path(), "home directory"),
        (&layout.home.join("Library"), "Library directory"),
        (
            &layout.home.join("Library/Application Support"),
            "Application Support directory",
        ),
        (&layout.root, "legacy data directory"),
        (&layout.archive, "legacy archive"),
        (
            layout.config.parent().expect("legacy config parent"),
            "legacy configuration directory",
        ),
        (&layout.runtime, "legacy runtime directory"),
        (&layout.program, "legacy program directory"),
        (
            layout.app.parent().expect("legacy app parent"),
            "Applications directory",
        ),
        (
            layout.login_plist.parent().expect("legacy login parent"),
            "LaunchAgents directory",
        ),
    ] {
        require_owned_directory(path, label)?;
    }
    require_owned_regular(&layout.config, "legacy configuration")?;
    require_exact(&layout.marker, MANAGED_MARKER, "legacy ownership marker")?;
    let allowed = mac_program_files();
    require_exact_children(&layout.program, &allowed)?;
    for name in allowed {
        require_owned_regular(&layout.program.join(name), "legacy program file")?;
    }
    let port = read_port(&layout.program.join("port"))?;
    let preference = fs::read_to_string(layout.program.join("start-at-login"))
        .map_err(|_| "The legacy login preference cannot be read.".to_owned())?;
    let launch_at_login = match preference.as_str() {
        "0\n" => false,
        "1\n" => true,
        _ => {
            return Err("The legacy start-at-login file must contain exactly 0 or 1. Restore that preference, then retry.".to_owned());
        }
    };
    let service = mac_service_contents(layout, port);
    require_exact(
        &layout.service_plist,
        service.as_bytes(),
        "legacy launchd service",
    )?;
    if launch_at_login {
        require_exact(
            &layout.login_plist,
            service.as_bytes(),
            "legacy login service",
        )?;
    } else if path_exists_without_following(&layout.login_plist)? {
        return Err("The legacy login plist exists while start-at-login is disabled. Remove only page.suchi.local.plist from Library/LaunchAgents, then retry.".to_owned());
    }
    validate_macos_bundle(
        &layout.app,
        "Suchi",
        "launch",
        &layout.program,
        &layout.home,
        false,
    )?;
    validate_macos_bundle(
        &layout.settings_app,
        "Suchi Settings",
        "settings",
        &layout.program,
        &layout.home,
        false,
    )?;
    Ok((port, launch_at_login))
}

#[cfg(target_os = "macos")]
fn validate_macos_files(layout: &MacLayout) -> Result<(u16, bool), String> {
    let (port, launch_at_login) = validate_macos_artifacts(layout)?;
    validate_launchd(layout, launch_at_login)?;
    Ok((port, launch_at_login))
}

#[cfg(target_os = "macos")]
fn validate_macos_resume(layout: &MacLayout, port: u16) -> Result<(), String> {
    for (path, label) in [
        (layout.home.as_path(), "home directory"),
        (&layout.home.join("Library"), "Library directory"),
        (
            &layout.home.join("Library/Application Support"),
            "Application Support directory",
        ),
        (&layout.root, "legacy data directory"),
        (&layout.archive, "legacy archive"),
        (
            layout.config.parent().expect("legacy config parent"),
            "legacy configuration directory",
        ),
        (&layout.runtime, "legacy runtime directory"),
    ] {
        require_owned_directory(path, label)?;
    }
    require_owned_regular(&layout.config, "legacy configuration")?;
    if path_exists_without_following(&layout.program)? {
        require_owned_directory(&layout.program, "legacy program directory")?;
        reject_unknown_children(&layout.program, &mac_program_files())?;
        if path_exists_without_following(&layout.marker)? {
            require_exact(&layout.marker, MANAGED_MARKER, "legacy ownership marker")?;
        }
        if path_exists_without_following(&layout.program.join("port"))?
            && read_port(&layout.program.join("port"))? != port
        {
            return Err("The legacy port changed during migration. Restore the retained backup or original port file, then retry.".to_owned());
        }
    }
    for (bundle, name, entry) in [
        (&layout.app, "Suchi", "launch"),
        (&layout.settings_app, "Suchi Settings", "settings"),
    ] {
        if path_exists_without_following(bundle)? {
            validate_macos_bundle(bundle, name, entry, &layout.program, &layout.home, true)?;
        }
    }
    Ok(())
}

#[cfg(target_os = "macos")]
fn validate_macos_bundle(
    bundle: &Path,
    name: &str,
    entry: &str,
    program: &Path,
    home: &Path,
    allow_partial: bool,
) -> Result<(), String> {
    if !allow_partial {
        require_owned_directory(bundle, "legacy app bundle")?;
    }
    let directories = [
        bundle.to_path_buf(),
        bundle.join("Contents"),
        bundle.join("Contents/MacOS"),
        bundle.join("Contents/Resources"),
    ];
    for directory in &directories {
        if path_exists_without_following(directory)? {
            require_owned_directory(directory, "legacy app bundle directory")?;
        } else if !allow_partial {
            return Err(format!(
                "The legacy app bundle is incomplete: {}. Reinstall the exact legacy package or remove its launcher without deleting the archive, then retry.",
                directory.display()
            ));
        }
    }
    if path_exists_without_following(&bundle.join("Contents"))? {
        reject_unknown_children(
            &bundle.join("Contents"),
            &["Info.plist", "MacOS", "Resources"],
        )?;
    }
    if path_exists_without_following(&bundle.join("Contents/MacOS"))? {
        reject_unknown_children(&bundle.join("Contents/MacOS"), &["Suchi"])?;
    }
    if path_exists_without_following(&bundle.join("Contents/Resources"))? {
        reject_unknown_children(&bundle.join("Contents/Resources"), &["Suchi.icns"])?;
    }
    let info = bundle.join("Contents/Info.plist");
    let executable = bundle.join("Contents/MacOS/Suchi");
    let icon = bundle.join("Contents/Resources/Suchi.icns");
    for (path, expected, label) in [
        (
            &info,
            mac_app_info(name, entry).into_bytes(),
            "legacy app metadata",
        ),
        (
            &executable,
            mac_app_executable(entry, program, home).into_bytes(),
            "legacy app launcher",
        ),
    ] {
        if path_exists_without_following(path)? {
            require_exact(path, &expected, label)?;
        } else if !allow_partial {
            return Err(format!("The {label} is missing at {}.", path.display()));
        }
    }
    if path_exists_without_following(&icon)? {
        require_owned_regular(&icon, "legacy app icon")?;
    } else if !allow_partial {
        return Err(format!(
            "The legacy app icon is missing at {}.",
            icon.display()
        ));
    }
    Ok(())
}

#[cfg(target_os = "macos")]
fn validate_launchd(layout: &MacLayout, launch_at_login: bool) -> Result<(), String> {
    let domain = format!("gui/{}", current_uid());
    let session = Command::new("/bin/launchctl")
        .args(["print", &domain])
        .output()
        .map_err(|_| "launchd could not be invoked.".to_owned())?;
    if !session.status.success() {
        return Err("The legacy launchd job cannot be inspected outside the desktop login session. Log in graphically, then retry.".to_owned());
    }
    let target = format!("{domain}/page.suchi.local");
    let output = Command::new("/bin/launchctl")
        .args(["print", &target])
        .output()
        .map_err(|_| "launchd could not inspect the legacy job.".to_owned())?;
    if !output.status.success() {
        return Ok(());
    }
    let expected = if launch_at_login {
        &layout.login_plist
    } else {
        &layout.service_plist
    };
    let report = String::from_utf8_lossy(&output.stdout);
    let loaded_path = report
        .lines()
        .find_map(|line| line.trim().strip_prefix("path = "))
        .ok_or_else(|| {
            "The loaded page.suchi.local launchd job does not report its source path. Boot it out manually, then retry.".to_owned()
        })?;
    let loaded = PathBuf::from(loaded_path);
    let loaded = fs::canonicalize(&loaded).map_err(|_| {
        "The loaded page.suchi.local launchd job points to a missing file. Boot it out manually, then retry.".to_owned()
    })?;
    let expected = fs::canonicalize(expected).map_err(|_| {
        "The expected legacy launchd service cannot be resolved. Reinstall the exact legacy package, then retry.".to_owned()
    })?;
    if loaded != expected {
        return Err("A foreign launchd job uses page.suchi.local. Boot out that conflicting job without deleting the archive, then retry.".to_owned());
    }
    Ok(())
}

#[cfg(target_os = "macos")]
fn stop_macos(layout: &MacLayout, launch_at_login: bool) -> Result<(), String> {
    validate_launchd(layout, launch_at_login)?;
    let domain = format!("gui/{}", current_uid());
    let target = format!("{domain}/page.suchi.local");
    let loaded = Command::new("/bin/launchctl")
        .args(["print", &target])
        .output()
        .map_err(|_| "launchd could not inspect the legacy job.".to_owned())?
        .status
        .success();
    if loaded {
        let output = Command::new("/bin/launchctl")
            .args(["bootout", &target])
            .output()
            .map_err(|_| "launchd could not stop the legacy job.".to_owned())?;
        if !output.status.success() {
            return Err("The legacy launchd job could not be stopped. Run `launchctl bootout gui/$(id -u)/page.suchi.local`, wait for it to exit, then retry.".to_owned());
        }
    }
    let output = Command::new("/bin/launchctl")
        .args(["disable", &target])
        .output()
        .map_err(|_| "launchd could not disable the legacy job.".to_owned())?;
    if !output.status.success() {
        return Err("The legacy launchd job could not be disabled. Disable page.suchi.local in the current GUI domain, then retry.".to_owned());
    }
    Ok(())
}

#[cfg(target_os = "macos")]
fn remove_macos(layout: &MacLayout, port: u16, launch_at_login: bool) -> Result<(), String> {
    let service = mac_service_contents(layout, port);
    if path_exists_without_following(&layout.service_plist)? {
        require_exact(
            &layout.service_plist,
            service.as_bytes(),
            "legacy launchd service",
        )?;
    }
    if path_exists_without_following(&layout.login_plist)? {
        if !launch_at_login {
            return Err("The legacy login plist appeared while migration was running. Remove only page.suchi.local.plist from Library/LaunchAgents, then retry.".to_owned());
        }
        require_exact(
            &layout.login_plist,
            service.as_bytes(),
            "legacy login service",
        )?;
        remove_owned_file(&layout.login_plist)?;
    }
    for (bundle, name, entry) in [
        (&layout.app, "Suchi", "launch"),
        (&layout.settings_app, "Suchi Settings", "settings"),
    ] {
        if path_exists_without_following(bundle)? {
            validate_macos_bundle(bundle, name, entry, &layout.program, &layout.home, true)?;
            for path in [
                bundle.join("Contents/MacOS/Suchi"),
                bundle.join("Contents/Info.plist"),
                bundle.join("Contents/Resources/Suchi.icns"),
            ] {
                remove_owned_file_if_present(&path)?;
            }
            for directory in [
                bundle.join("Contents/MacOS"),
                bundle.join("Contents/Resources"),
                bundle.join("Contents"),
                bundle.to_path_buf(),
            ] {
                if path_exists_without_following(&directory)? {
                    fs::remove_dir(&directory).map_err(|_| format!("The legacy app bundle contains an unknown file: {}. Remove only the old Suchi launcher bundle, then retry.", directory.display()))?;
                }
            }
        }
    }
    if path_exists_without_following(&layout.program)? {
        reject_unknown_children(&layout.program, &mac_program_files())?;
        for name in mac_program_files() {
            remove_owned_file_if_present(&layout.program.join(name))?;
        }
        fs::remove_dir(&layout.program).map_err(|_| format!("The legacy program directory contains an unknown file: {}. Remove only the installer-owned files, then retry.", layout.program.display()))?;
    }
    remove_owned_file_if_present(&layout.runtime.join("setup-token"))?;
    Ok(())
}

#[cfg(target_os = "macos")]
fn mac_program_files() -> [&'static str; 11] {
    [
        "suchi",
        "install.sh",
        "launch.sh",
        "common.sh",
        "settings.sh",
        "icon.png",
        "Suchi.icns",
        "page.suchi.local.plist",
        "port",
        "start-at-login",
        "managed-by-suchi-local",
    ]
}

#[cfg(target_os = "macos")]
fn mac_service_contents(layout: &MacLayout, port: u16) -> String {
    format!(
        "<?xml version=\"1.0\" encoding=\"UTF-8\"?>\n<!-- Managed by Suchi local installer. -->\n<!DOCTYPE plist PUBLIC \"-//Apple//DTD PLIST 1.0//EN\" \"http://www.apple.com/DTDs/PropertyList-1.0.dtd\">\n<plist version=\"1.0\"><dict>\n<key>Label</key><string>page.suchi.local</string>\n<key>ProgramArguments</key><array>\n<string>/usr/bin/env</string><string>-i</string>\n<string>HOME={}</string>\n<string>PATH=/opt/homebrew/bin:/usr/local/bin:/usr/bin:/bin:/usr/sbin:/sbin</string>\n<string>DATA_DIR={}</string>\n<string>SUCHI_CONFIG={}</string>\n<string>LISTEN_ADDR=127.0.0.1:{port}</string>\n<string>PUBLIC_URL=http://127.0.0.1:{port}</string>\n<string>SUCHI_DEV=0</string><string>SUCHI_DEMO_MODE=0</string>\n<string>{}/suchi</string><string>serve</string>\n<string>--setup-token-file={}/setup-token</string>\n</array>\n<key>WorkingDirectory</key><string>{}</string>\n<key>KeepAlive</key><true/>\n<key>ThrottleInterval</key><integer>10</integer>\n<key>ExitTimeOut</key><integer>60</integer>\n<key>Umask</key><integer>63</integer>\n<key>StandardOutPath</key><string>{}/server.log</string>\n<key>StandardErrorPath</key><string>{}/server.log</string>\n</dict></plist>\n",
        layout.home.display(),
        layout.archive.display(),
        layout.config.display(),
        layout.program.display(),
        layout.runtime.display(),
        layout.archive.display(),
        layout.runtime.display(),
        layout.runtime.display(),
    )
}

#[cfg(target_os = "macos")]
fn mac_app_info(name: &str, entry: &str) -> String {
    format!(
        "<?xml version=\"1.0\" encoding=\"UTF-8\"?>\n<!-- Managed by Suchi local installer. -->\n<plist version=\"1.0\"><dict>\n<key>CFBundleIdentifier</key><string>page.suchi.{entry}</string>\n<key>CFBundleName</key><string>{name}</string>\n<key>CFBundleExecutable</key><string>Suchi</string>\n<key>CFBundlePackageType</key><string>APPL</string>\n<key>CFBundleVersion</key><string>1</string>\n<key>CFBundleIconFile</key><string>Suchi.icns</string>\n<key>LSUIElement</key><true/>\n</dict></plist>\n"
    )
}

#[cfg(target_os = "macos")]
fn mac_app_executable(entry: &str, program: &Path, home: &Path) -> String {
    format!(
        "#!/bin/bash\n# Managed by Suchi local installer.\nexec /usr/bin/env -i 'HOME={}' 'PATH=/usr/bin:/bin:/usr/sbin:/sbin' /bin/bash '{}/{entry}.sh'\n",
        home.display(),
        program.display(),
    )
}

fn read_port(path: &Path) -> Result<u16, String> {
    require_owned_regular(path, "legacy port file")?;
    let raw =
        fs::read_to_string(path).map_err(|_| "The legacy port file cannot be read.".to_owned())?;
    let port = raw
        .strip_suffix('\n')
        .filter(|value| !value.is_empty() && value.bytes().all(|byte| byte.is_ascii_digit()))
        .and_then(|value| value.parse::<u16>().ok())
        .ok_or_else(|| {
            "The legacy port file must contain exactly one integer from 1024 through 65535 followed by a newline."
                .to_owned()
        })?;
    if port < 1024 {
        return Err("The legacy port must be an integer from 1024 through 65535.".to_owned());
    }
    Ok(port)
}

fn wait_for_free_port(port: u16) -> Result<(), String> {
    let deadline = Instant::now() + PORT_WAIT;
    while Instant::now() < deadline {
        match TcpListener::bind(("127.0.0.1", port)) {
            Ok(listener) => {
                drop(listener);
                return Ok(());
            }
            Err(_) => thread::sleep(Duration::from_millis(250)),
        }
    }
    Err(format!(
        "Port {port} is still owned after the legacy service was stopped. Identify and stop that listener; Suchi will not delete legacy files or choose another port."
    ))
}

fn require_exact_children<const N: usize>(
    directory: &Path,
    allowed: &[&str; N],
) -> Result<(), String> {
    reject_unknown_children(directory, allowed)?;
    let actual = fs::read_dir(directory)
        .map_err(|_| {
            format!(
                "The legacy directory cannot be read: {}.",
                directory.display()
            )
        })?
        .count();
    if actual != N {
        return Err(format!(
            "The legacy installation is incomplete in {}. Reinstall the exact legacy package without deleting the archive, then retry.",
            directory.display()
        ));
    }
    Ok(())
}

fn reject_unknown_children<const N: usize>(
    directory: &Path,
    allowed: &[&str; N],
) -> Result<(), String> {
    for entry in fs::read_dir(directory).map_err(|_| {
        format!(
            "The legacy directory cannot be read: {}.",
            directory.display()
        )
    })? {
        let entry = entry.map_err(|_| {
            format!(
                "A legacy directory entry cannot be inspected in {}.",
                directory.display()
            )
        })?;
        if !allowed
            .iter()
            .any(|allowed_name| entry.file_name() == OsStr::new(allowed_name))
        {
            return Err(format!(
                "The legacy program or launcher directory contains a foreign entry: {}. Move that entry aside without deleting the archive, then retry.",
                entry.path().display()
            ));
        }
    }
    Ok(())
}

fn require_exact(path: &Path, expected: &[u8], label: &str) -> Result<(), String> {
    let metadata = require_owned_regular(path, label)?;
    if metadata.len() > 1024 * 1024 {
        return Err(format!(
            "The {label} is unexpectedly large: {}.",
            path.display()
        ));
    }
    let actual =
        fs::read(path).map_err(|_| format!("The {label} cannot be read: {}.", path.display()))?;
    if actual != expected {
        return Err(format!(
            "The {label} does not match the exact managed installer contract: {}. Restore the original managed file or remove the legacy integration without deleting the archive, then retry.",
            path.display()
        ));
    }
    Ok(())
}

fn require_owned_regular(path: &Path, label: &str) -> Result<fs::Metadata, String> {
    let metadata = fs::symlink_metadata(path).map_err(|_| {
        format!(
            "The {label} is missing or inaccessible: {}.",
            path.display()
        )
    })?;
    if metadata.file_type().is_symlink() || !metadata.is_file() || !owned_by_current_user(&metadata)
    {
        return Err(format!(
            "The {label} is a symlink, non-file, or owned by another user: {}. Restore an ordinary file owned by this desktop user, then retry.",
            path.display()
        ));
    }
    Ok(metadata)
}

fn require_owned_directory(path: &Path, label: &str) -> Result<(), String> {
    let metadata = fs::symlink_metadata(path).map_err(|_| {
        format!(
            "The {label} is missing or inaccessible: {}.",
            path.display()
        )
    })?;
    if metadata.file_type().is_symlink() || !metadata.is_dir() || !owned_by_current_user(&metadata)
    {
        return Err(format!(
            "The {label} is a symlink, non-directory, or owned by another user: {}. Restore an ordinary directory owned by this desktop user, then retry.",
            path.display()
        ));
    }
    Ok(())
}

fn remove_owned_file_if_present(path: &Path) -> Result<(), String> {
    if path_exists_without_following(path)? {
        remove_owned_file(path)?;
    }
    Ok(())
}

fn remove_owned_file(path: &Path) -> Result<(), String> {
    require_owned_regular(path, "legacy installer-owned file")?;
    fs::remove_file(path).map_err(|_| {
        format!(
            "The legacy installer-owned file could not be removed: {}. Fix its permissions, then retry.",
            path.display()
        )
    })
}

fn path_exists_without_following(path: &Path) -> Result<bool, String> {
    match fs::symlink_metadata(path) {
        Ok(_) => Ok(true),
        Err(error) if error.kind() == std::io::ErrorKind::NotFound => Ok(false),
        Err(_) => Err(format!(
            "The legacy path cannot be inspected: {}.",
            path.display()
        )),
    }
}

#[cfg(unix)]
fn current_uid() -> u32 {
    // SAFETY: geteuid takes no pointers and has no preconditions.
    unsafe { libc::geteuid() }
}

#[cfg(unix)]
fn owned_by_current_user(metadata: &fs::Metadata) -> bool {
    use std::os::unix::fs::MetadataExt;
    metadata.uid() == current_uid()
}

#[cfg(not(unix))]
fn owned_by_current_user(_metadata: &fs::Metadata) -> bool {
    false
}

#[cfg(all(test, target_os = "macos"))]
mod tests {
    use super::legacy_fixtures::LegacyMacFixture;
    use super::*;
    use std::time::{SystemTime, UNIX_EPOCH};

    #[test]
    fn exact_legacy_macos_artifacts_are_accepted_and_foreign_entries_block() {
        let unique = SystemTime::now()
            .duration_since(UNIX_EPOCH)
            .expect("clock after epoch")
            .as_nanos();
        let home =
            std::env::temp_dir().join(format!("suchi-legacy-test-{}-{unique}", std::process::id()));
        let fixture = LegacyMacFixture::build(&home, 18_765, false);
        for directory in [
            fixture.archive.clone(),
            fixture
                .config
                .parent()
                .expect("config parent")
                .to_path_buf(),
            fixture.runtime.clone(),
            fixture.program.clone(),
            fixture
                .login_plist
                .parent()
                .expect("login plist parent")
                .to_path_buf(),
            fixture.app.join("Contents/MacOS"),
            fixture.app.join("Contents/Resources"),
            fixture.settings_app.join("Contents/MacOS"),
            fixture.settings_app.join("Contents/Resources"),
        ] {
            fs::create_dir_all(directory).expect("create fixture directory");
        }
        fs::write(&fixture.config, b"# legacy config\n").expect("write config");
        for path in &fixture.owned_program_files {
            fs::write(path, b"legacy\n").expect("write owned program file");
        }
        fs::write(&fixture.marker, MANAGED_MARKER).expect("write marker");
        fs::write(&fixture.port_file, b"18765\n").expect("write port");
        fs::write(&fixture.login_preference_file, b"0\n").expect("write login preference");
        fs::write(&fixture.service_plist, &fixture.service_contents).expect("write service");
        for (bundle, name, entry) in [
            (&fixture.app, "Suchi", "launch"),
            (&fixture.settings_app, "Suchi Settings", "settings"),
        ] {
            fs::write(
                bundle.join("Contents/Info.plist"),
                mac_app_info(name, entry),
            )
            .expect("write app metadata");
            fs::write(
                bundle.join("Contents/MacOS/Suchi"),
                mac_app_executable(entry, &fixture.program, &home),
            )
            .expect("write app launcher");
            fs::write(bundle.join("Contents/Resources/Suchi.icns"), b"icon")
                .expect("write app icon");
        }
        let layout = MacLayout {
            home: home.clone(),
            root: fixture.root.clone(),
            archive: fixture.archive.clone(),
            config: fixture.config.clone(),
            runtime: fixture.runtime.clone(),
            program: fixture.program.clone(),
            marker: fixture.marker.clone(),
            service_plist: fixture.service_plist.clone(),
            login_plist: fixture.login_plist.clone(),
            app: fixture.app.clone(),
            settings_app: fixture.settings_app.clone(),
        };

        assert_eq!(
            validate_macos_artifacts(&layout).expect("exact fixture accepted"),
            (18_765, false)
        );

        let foreign = fixture.program.join("foreign");
        fs::write(&foreign, b"not managed").expect("write foreign file");
        let error = validate_macos_artifacts(&layout).expect_err("foreign entry rejected");
        assert!(error.contains("foreign entry"), "unexpected error: {error}");

        fs::remove_dir_all(home).expect("remove fixture");
    }
}
