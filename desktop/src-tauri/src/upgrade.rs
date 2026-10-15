// SPDX-License-Identifier: AGPL-3.0-or-later

use crate::{
    backup,
    desktop_state::{BuildIdentity, OperationKind, PendingOperation},
    process::DesktopState,
};
use semver::Version;
use std::{fs, path::Path};
use tauri::AppHandle;
use tauri_plugin_dialog::DialogExt;

pub fn prepare(app: &AppHandle, state: &DesktopState, executable: &Path) -> Result<(), String> {
    let current = state.build_identity();
    let snapshot = state.state_store().snapshot();
    if let Some(pending) = snapshot.pending_operation {
        if pending.target_build != current {
            return Err(
                "A pending archive operation belongs to a different installed build. Reinstall that build to retry, or restore its retained backup and quit."
                    .to_owned(),
            );
        }
        return Ok(());
    }

    if let Some(previous) = &snapshot.last_ready_build {
        if previous == &current {
            return Ok(());
        }
        if is_older(&current, previous) {
            return Err(
                "This installed Suchi version is older than the last version that opened the archive. Restore a complete backup from the older version before launching it."
                    .to_owned(),
            );
        }
    }
    if !archive_has_database(state.paths().archive.join("suchi.db"))? {
        return Ok(());
    }

    let _guard = state.begin_operation()?;
    let source = snapshot.last_ready_build.unwrap_or_else(|| BuildIdentity {
        version: "unrecorded".to_owned(),
        revision: "unknown".to_owned(),
    });
    let default_destination = backup::automatic_destination(state, "before-upgrade");
    let verified = match backup::create_verified(state, executable, &default_destination, false) {
        Ok(verified) => verified,
        Err(message) if message.starts_with("The backup destination needs approximately") => {
            let selected = app
                .dialog()
                .file()
                .set_title("Choose External Pre-upgrade Backup Destination")
                .set_file_name(format!(
                    "suchi-before-upgrade-{}.suchi-backup",
                    std::process::id()
                ))
                .add_filter("Suchi complete backup", &["suchi-backup"])
                .blocking_save_file()
                .ok_or_else(|| {
                    "The upgrade requires a verified complete backup. Choose an external destination with enough free space, then retry."
                        .to_owned()
                })?
                .into_path()
                .map_err(|_| "The selected external backup destination is invalid.".to_owned())?;
            backup::create_verified(state, executable, &selected, true)?
        }
        Err(message) => return Err(message),
    };

    state.state_store().update(|desktop| {
        desktop.pending_operation = Some(PendingOperation {
            kind: OperationKind::Upgrade,
            source_build: source,
            target_build: current,
            backup_path: verified.path,
            backup_sha256: verified.sha256,
            restore_source: None,
            restore_id: None,
        });
        Ok(())
    })?;
    Ok(())
}

pub fn installed_build_is_older(state: &DesktopState) -> bool {
    let current = state.build_identity();
    state
        .state_store()
        .snapshot()
        .last_ready_build
        .as_ref()
        .is_some_and(|previous| is_older(&current, previous))
}

fn archive_has_database(path: impl AsRef<Path>) -> Result<bool, String> {
    let path = path.as_ref();
    let metadata = match fs::symlink_metadata(path) {
        Ok(metadata) => metadata,
        Err(error) if error.kind() == std::io::ErrorKind::NotFound => return Ok(false),
        Err(_) => return Err("The archive database cannot be inspected before upgrade.".to_owned()),
    };
    if metadata.file_type().is_symlink() || !metadata.is_file() {
        return Err("The archive database has an unsafe file type.".to_owned());
    }
    Ok(metadata.len() > 0)
}

fn is_older(current: &BuildIdentity, previous: &BuildIdentity) -> bool {
    let current = Version::parse(current.version.trim_start_matches('v'));
    let previous = Version::parse(previous.version.trim_start_matches('v'));
    matches!((current, previous), (Ok(current), Ok(previous)) if current < previous)
}

#[cfg(test)]
mod tests {
    use super::*;

    fn build(version: &str) -> BuildIdentity {
        BuildIdentity {
            version: version.to_owned(),
            revision: "revision".to_owned(),
        }
    }

    #[test]
    fn semantic_downgrade_is_refused() {
        assert!(is_older(&build("v0.2.9"), &build("v0.3.0")));
        assert!(!is_older(&build("v0.3.0"), &build("v0.3.0")));
        assert!(!is_older(&build("v0.4.0"), &build("v0.3.0")));
    }
}
