// SPDX-License-Identifier: AGPL-3.0-or-later

use crate::{
    desktop_state::{BuildIdentity, OperationKind, PendingOperation},
    navigation,
    process::DesktopState,
    sidecar_cli, upgrade,
};
use serde::Deserialize;
use std::{
    ffi::OsString,
    fs,
    path::{Path, PathBuf},
    thread,
    time::{Duration, SystemTime, UNIX_EPOCH},
};
use tauri::AppHandle;
use tauri_plugin_dialog::DialogExt;

const STOP_WAIT: Duration = Duration::from_secs(22);

#[derive(Debug, Deserialize)]
struct PreflightReport {
    version: u8,
    archive_bytes: i64,
    issues: Vec<String>,
}

#[derive(Debug, Deserialize)]
struct BackupManifest {
    build_version: String,
    build_revision: String,
}

#[derive(Debug, Deserialize)]
struct CreateReport {
    version: u8,
    path: PathBuf,
    sha256: String,
}

#[derive(Debug, Deserialize)]
struct VerifyReport {
    version: u8,
    path: PathBuf,
    sha256: String,
    manifest: BackupManifest,
}

#[derive(Debug, Deserialize)]
struct RestoreReport {
    version: u8,
    id: String,
    phase: String,
}

#[derive(Debug, Deserialize)]
struct RecoveryReport {
    version: u8,
    pending: bool,
    id: Option<String>,
}

#[derive(Clone, Debug)]
pub struct VerifiedBackup {
    pub path: PathBuf,
    pub sha256: String,
    pub source_build: BuildIdentity,
}

pub fn choose_complete_backup(app: &AppHandle, state: DesktopState) {
    let selected_app = app.clone();
    let default_name = format!("suchi-complete-{}.suchi-backup", unix_timestamp());
    app.dialog()
        .file()
        .set_title("Create Complete Suchi Backup")
        .set_directory(&state.paths().backups)
        .set_file_name(default_name)
        .add_filter("Suchi complete backup", &["suchi-backup"])
        .save_file(move |selection| {
            let Some(selection) = selection else {
                return;
            };
            let Ok(destination) = selection.into_path() else {
                return;
            };
            thread::spawn(move || run_manual_backup(&selected_app, &state, &destination));
        });
}

pub fn choose_complete_restore(app: &AppHandle, state: DesktopState) {
    let selected_app = app.clone();
    app.dialog()
        .file()
        .set_title("Restore Complete Suchi Backup")
        .set_directory(&state.paths().backups)
        .add_filter("Suchi complete backup", &["suchi-backup"])
        .pick_file(move |selection| {
            let Some(selection) = selection else {
                return;
            };
            let Ok(source) = selection.into_path() else {
                return;
            };
            thread::spawn(move || run_manual_restore(&selected_app, &state, &source));
        });
}

pub fn create_verified(
    state: &DesktopState,
    executable: &Path,
    destination: &Path,
    overwrite: bool,
) -> Result<VerifiedBackup, String> {
    let preflight_arguments = os_arguments(["backup", "preflight", "--json"]);
    let preflight = sidecar_cli::run_json::<PreflightReport>(
        executable,
        state.paths(),
        state.port(),
        &preflight_arguments,
        "backup preflight",
    )?;
    if preflight.value.version != 1 {
        return Err("The backup preflight report version is unsupported.".to_owned());
    }
    let blocking_issues = preflight
        .value
        .issues
        .iter()
        .filter(|issue| {
            !issue.starts_with("insufficient free space:")
                && !issue.starts_with("inspect free space:")
        })
        .take(3)
        .cloned()
        .collect::<Vec<_>>();
    if !blocking_issues.is_empty() {
        return Err(format!(
            "The archive is not ready for a complete backup: {}",
            blocking_issues.join("; ")
        ));
    }
    ensure_destination_space(destination, preflight.value.archive_bytes)?;

    let mut create_arguments = vec![
        OsString::from("backup"),
        OsString::from("create"),
        OsString::from("--out"),
        destination.as_os_str().to_owned(),
        OsString::from("--json"),
    ];
    if overwrite {
        create_arguments.push(OsString::from("--overwrite"));
    }
    let created = sidecar_cli::run_json::<CreateReport>(
        executable,
        state.paths(),
        state.port(),
        &create_arguments,
        "complete backup",
    )?;
    if !created.success || created.value.version != 1 {
        return Err("The complete backup could not be created.".to_owned());
    }

    let verify_arguments = vec![
        OsString::from("backup"),
        OsString::from("verify"),
        destination.as_os_str().to_owned(),
        OsString::from("--json"),
    ];
    let verified = sidecar_cli::run_json::<VerifyReport>(
        executable,
        state.paths(),
        state.port(),
        &verify_arguments,
        "backup verification",
    )?;
    if !verified.success
        || verified.value.version != 1
        || created.value.sha256 != verified.value.sha256
        || created.value.path != verified.value.path
    {
        return Err("The new complete backup failed verification.".to_owned());
    }
    Ok(VerifiedBackup {
        path: verified.value.path,
        sha256: verified.value.sha256,
        source_build: BuildIdentity {
            version: verified.value.manifest.build_version,
            revision: verified.value.manifest.build_revision,
        },
    })
}
pub fn finalize_restore_after_ready(
    app: AppHandle,
    state: DesktopState,
    pending: PendingOperation,
) {
    thread::spawn(move || {
        let result = (|| {
            let _guard = state.begin_operation()?;
            state.wait_until_stopped(STOP_WAIT)?;
            state.set_maintenance("Committing the verified restored archive…")?;
            let executable = state.sidecar_executable()?;
            let restore_id = pending
                .restore_id
                .as_deref()
                .ok_or_else(|| "The pending restore has no recovery identifier.".to_owned())?;
            commit_recovery(&state, &executable, restore_id)?;
            state.state_store().update(|desktop| {
                desktop.pending_operation = None;
                desktop.last_ready_build = Some(pending.target_build);
                Ok(())
            })?;
            state.finish_maintenance();
            state.start(&app)
        })();
        if let Err(message) = result {
            state.log_operation_error("restore-finalize", &message);
            state.set_error(message);
            navigation::navigate_to_launcher(&app);
        }
    });
}

pub fn automatic_destination(state: &DesktopState, purpose: &str) -> PathBuf {
    state
        .paths()
        .backups
        .join(format!("suchi-{purpose}-{}.suchi-backup", unix_timestamp()))
}

pub fn resume_pending_restore(
    app: &AppHandle,
    state: &DesktopState,
    executable: &Path,
) -> Result<bool, String> {
    let Some(pending) = state
        .state_store()
        .snapshot()
        .pending_operation
        .filter(|pending| {
            matches!(
                pending.kind,
                OperationKind::Restore | OperationKind::DowngradeRestore
            )
        })
    else {
        return Ok(false);
    };
    if pending.kind == OperationKind::Restore && pending.target_build != state.build_identity() {
        return Err(
            "The pending restore belongs to a different installed build. Reinstall that build to finish or roll back the restore."
                .to_owned(),
        );
    }
    let arguments = os_arguments(["backup", "recover", "--status", "--json"]);
    let recovery = sidecar_cli::run_json::<RecoveryReport>(
        executable,
        state.paths(),
        state.port(),
        &arguments,
        "restore recovery status",
    )?;
    if !recovery.success || recovery.value.version != 1 {
        return Err("The restore recovery status could not be read.".to_owned());
    }
    let restore_id = if recovery.value.pending {
        let id = recovery
            .value
            .id
            .ok_or_else(|| "The pending restore journal has no recovery identifier.".to_owned())?;
        if pending
            .restore_id
            .as_ref()
            .is_some_and(|expected| expected != &id)
        {
            return Err("The restore journal does not match the desktop transaction. Restore the retained backup before continuing.".to_owned());
        }
        id
    } else {
        let source = pending
            .restore_source
            .as_deref()
            .ok_or_else(|| "The pending restore record has no selected backup path.".to_owned())?;
        verify_existing(state, executable, source)?;
        activate_backup(state, executable, source, "resumed complete restore")?.id
    };
    state.state_store().update(|desktop| {
        let current = desktop
            .pending_operation
            .as_mut()
            .filter(|current| current.kind == pending.kind)
            .ok_or_else(|| "The pending restore record disappeared during recovery.".to_owned())?;
        current.restore_id = Some(restore_id.clone());
        Ok(())
    })?;
    if pending.kind == OperationKind::DowngradeRestore {
        commit_recovery(state, executable, &restore_id)?;
        state.state_store().update(|desktop| {
            desktop.pending_operation = None;
            desktop.last_ready_build = Some(pending.target_build);
            Ok(())
        })?;
        state.finish_maintenance();
        state.begin_quit(app.clone());
        return Ok(true);
    }
    Ok(false)
}

pub fn rollback_pending(app: AppHandle, state: DesktopState) {
    thread::spawn(move || {
        if let Err(message) = rollback_pending_inner(&app, &state) {
            state.log_operation_error("recovery", &message);
            state.set_error(message);
            navigation::navigate_to_launcher(&app);
        }
    });
}

fn run_manual_backup(app: &AppHandle, state: &DesktopState, destination: &Path) {
    let result = (|| {
        let _guard = state.begin_operation()?;
        state.stop();
        state.wait_until_stopped(STOP_WAIT)?;
        state.set_maintenance("Creating and verifying a complete backup…")?;
        let executable = state.sidecar_executable()?;
        create_verified(state, &executable, destination, true)?;
        state.finish_maintenance();
        state.start(app)
    })();
    if let Err(message) = result {
        state.log_operation_error("backup", &message);
        state.set_error(message);
        navigation::navigate_to_launcher(app);
    }
}

fn run_manual_restore(app: &AppHandle, state: &DesktopState, source: &Path) {
    let result = (|| {
        let _guard = state.begin_operation()?;
        state.stop();
        state.wait_until_stopped(STOP_WAIT)?;
        let executable = state.sidecar_executable()?;

        if upgrade::installed_build_is_older(state) {
            state.set_maintenance("Verifying the downgrade backup…")?;
            let selected = verify_existing(state, &executable, source)?;
            let source_build = state
                .state_store()
                .snapshot()
                .last_ready_build
                .unwrap_or_else(|| state.build_identity());
            let target_build = selected.source_build;
            state.state_store().update(|desktop| {
                desktop.pending_operation = Some(PendingOperation {
                    kind: OperationKind::DowngradeRestore,
                    source_build,
                    target_build: target_build.clone(),
                    backup_path: selected.path.clone(),
                    backup_sha256: selected.sha256.clone(),
                    restore_source: Some(selected.path.clone()),
                    restore_id: None,
                });
                Ok(())
            })?;
            state.set_maintenance("Restoring the downgrade archive without launching it…")?;
            let restored = activate_backup(state, &executable, source, "downgrade restore")?;
            state.state_store().update(|desktop| {
                let pending = desktop
                    .pending_operation
                    .as_mut()
                    .filter(|pending| pending.kind == OperationKind::DowngradeRestore)
                    .ok_or_else(|| {
                        "The pending downgrade restore disappeared during activation.".to_owned()
                    })?;
                pending.restore_id = Some(restored.id.clone());
                Ok(())
            })?;
            commit_recovery(state, &executable, &restored.id)?;
            state.state_store().update(|desktop| {
                desktop.pending_operation = None;
                desktop.last_ready_build = Some(target_build);
                Ok(())
            })?;
            state.finish_maintenance();
            state.begin_quit(app.clone());
            return Ok(());
        }

        state.set_maintenance("Creating a retained backup before restore…")?;
        let retained_path = automatic_destination(state, "before-restore");
        let retained = create_verified(state, &executable, &retained_path, false)?;

        state.set_maintenance("Verifying and activating the selected complete backup…")?;
        verify_existing(state, &executable, source)?;
        let source_build = state
            .state_store()
            .snapshot()
            .last_ready_build
            .unwrap_or_else(|| state.build_identity());
        let target_build = state.build_identity();
        state.state_store().update(|desktop| {
            desktop.pending_operation = Some(PendingOperation {
                kind: OperationKind::Restore,
                source_build,
                target_build,
                backup_path: retained.path,
                backup_sha256: retained.sha256,
                restore_source: Some(source.to_path_buf()),
                restore_id: None,
            });
            Ok(())
        })?;
        let restored = activate_backup(state, &executable, source, "complete restore")?;
        state.state_store().update(|desktop| {
            let pending = desktop
                .pending_operation
                .as_mut()
                .filter(|pending| pending.kind == OperationKind::Restore)
                .ok_or_else(|| {
                    "The pending restore record disappeared during activation.".to_owned()
                })?;
            pending.restore_id = Some(restored.id);
            Ok(())
        })?;
        state.finish_maintenance();
        state.start(app)
    })();
    if let Err(message) = result {
        state.log_operation_error("restore", &message);
        state.set_error(message);
        navigation::navigate_to_launcher(app);
    }
}

fn activate_backup(
    state: &DesktopState,
    executable: &Path,
    source: &Path,
    operation: &str,
) -> Result<RestoreReport, String> {
    let restore_arguments = vec![
        OsString::from("backup"),
        OsString::from("restore"),
        source.as_os_str().to_owned(),
        OsString::from("--json"),
    ];
    let restored = sidecar_cli::run_json::<RestoreReport>(
        executable,
        state.paths(),
        state.port(),
        &restore_arguments,
        operation,
    )?;
    if !restored.success || restored.value.version != 1 || restored.value.phase != "activated" {
        return Err("The selected complete backup could not be activated.".to_owned());
    }
    Ok(restored.value)
}

fn verify_existing(
    state: &DesktopState,
    executable: &Path,
    source: &Path,
) -> Result<VerifiedBackup, String> {
    let arguments = vec![
        OsString::from("backup"),
        OsString::from("verify"),
        source.as_os_str().to_owned(),
        OsString::from("--json"),
    ];
    let verified = sidecar_cli::run_json::<VerifyReport>(
        executable,
        state.paths(),
        state.port(),
        &arguments,
        "backup verification",
    )?;
    if !verified.success || verified.value.version != 1 {
        return Err("The selected complete backup failed verification.".to_owned());
    }
    Ok(VerifiedBackup {
        path: verified.value.path,
        sha256: verified.value.sha256,
        source_build: BuildIdentity {
            version: verified.value.manifest.build_version,
            revision: verified.value.manifest.build_revision,
        },
    })
}

fn rollback_pending_inner(app: &AppHandle, state: &DesktopState) -> Result<(), String> {
    let _guard = state.begin_operation()?;
    state.stop();
    state.wait_until_stopped(STOP_WAIT)?;
    state.set_maintenance("Rolling back the pending archive operation…")?;
    let pending = state
        .state_store()
        .snapshot()
        .pending_operation
        .ok_or_else(|| "There is no pending archive operation to roll back.".to_owned())?;
    let executable = state.sidecar_executable()?;
    match pending.kind {
        OperationKind::Restore | OperationKind::DowngradeRestore => {
            let arguments = os_arguments(["backup", "recover", "--rollback", "--json"]);
            let recovery = sidecar_cli::run_json::<RecoveryReport>(
                &executable,
                state.paths(),
                state.port(),
                &arguments,
                "restore rollback",
            )?;
            if !recovery.success || recovery.value.version != 1 || recovery.value.pending {
                return Err("The previous archive state could not be restored.".to_owned());
            }
            state.state_store().update(|desktop| {
                desktop.pending_operation = None;
                desktop.last_ready_build = Some(pending.source_build);
                Ok(())
            })?;
            state.finish_maintenance();
            if pending.kind == OperationKind::DowngradeRestore {
                state.begin_quit(app.clone());
                Ok(())
            } else {
                state.start(app)
            }
        }
        OperationKind::Upgrade | OperationKind::LegacyMigration => {
            let restored = activate_backup(
                state,
                &executable,
                &pending.backup_path,
                "pre-operation restore",
            )?;
            commit_recovery(state, &executable, &restored.id)?;
            state.state_store().update(|desktop| {
                desktop.pending_operation = None;
                desktop.last_ready_build = Some(pending.source_build);
                if pending.kind == OperationKind::LegacyMigration {
                    desktop.legacy_migration_complete = false;
                }
                Ok(())
            })?;
            state.finish_maintenance();
            state.begin_quit(app.clone());
            Ok(())
        }
    }
}

pub fn commit_recovery(
    state: &DesktopState,
    executable: &Path,
    restore_id: &str,
) -> Result<(), String> {
    let arguments = vec![
        OsString::from("backup"),
        OsString::from("recover"),
        OsString::from("--commit"),
        OsString::from(restore_id),
        OsString::from("--json"),
    ];
    let recovery = sidecar_cli::run_json::<RecoveryReport>(
        executable,
        state.paths(),
        state.port(),
        &arguments,
        "restore commit",
    )?;
    if !recovery.success || recovery.value.version != 1 || recovery.value.pending {
        return Err("The restored archive could not be committed.".to_owned());
    }
    Ok(())
}

fn ensure_destination_space(destination: &Path, required: i64) -> Result<(), String> {
    if required < 0 {
        return Err("The backup size estimate is invalid.".to_owned());
    }
    let parent = destination
        .parent()
        .ok_or_else(|| "The backup destination has no parent directory.".to_owned())?;
    let metadata = fs::symlink_metadata(parent)
        .map_err(|_| "The backup destination directory is unavailable.".to_owned())?;
    if metadata.file_type().is_symlink() || !metadata.is_dir() {
        return Err("The backup destination directory has an unsafe file type.".to_owned());
    }
    let available = fs2::available_space(parent)
        .map_err(|_| "The backup destination free space cannot be inspected.".to_owned())?;
    if required as u64 > available {
        return Err(format!(
            "The backup destination needs approximately {required} bytes but only {available} bytes are available. Choose an external destination with more space."
        ));
    }
    Ok(())
}

fn os_arguments<const N: usize>(arguments: [&str; N]) -> Vec<OsString> {
    arguments.into_iter().map(OsString::from).collect()
}

fn unix_timestamp() -> u64 {
    SystemTime::now()
        .duration_since(UNIX_EPOCH)
        .unwrap_or_default()
        .as_secs()
}
