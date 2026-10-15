// SPDX-License-Identifier: AGPL-3.0-or-later

use crate::{identity::SidecarIdentity, paths::atomic_write_private};
use serde::{Deserialize, Serialize};
use std::{
    fs,
    path::{Path, PathBuf},
    sync::{Arc, Mutex},
};

const PROTOCOL_VERSION: u8 = 1;
const STATE_LIMIT: u64 = 64 * 1024;

#[derive(Clone, Debug, Deserialize, Eq, PartialEq, Serialize)]
#[serde(deny_unknown_fields)]
pub struct BuildIdentity {
    pub version: String,
    pub revision: String,
}

impl From<&SidecarIdentity> for BuildIdentity {
    fn from(identity: &SidecarIdentity) -> Self {
        Self {
            version: identity.version.clone(),
            revision: identity.revision.clone(),
        }
    }
}

#[derive(Clone, Copy, Debug, Deserialize, Eq, PartialEq, Serialize)]
#[serde(rename_all = "snake_case")]
pub enum OperationKind {
    Upgrade,
    Restore,
    DowngradeRestore,
    LegacyMigration,
}

#[derive(Clone, Debug, Deserialize, Eq, PartialEq, Serialize)]
#[serde(deny_unknown_fields)]
pub struct PendingOperation {
    pub kind: OperationKind,
    pub source_build: BuildIdentity,
    pub target_build: BuildIdentity,
    pub backup_path: PathBuf,
    pub backup_sha256: String,
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub restore_source: Option<PathBuf>,
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub restore_id: Option<String>,
}

#[derive(Clone, Debug, Deserialize, Eq, PartialEq, Serialize)]
#[serde(deny_unknown_fields)]
pub struct DesktopStateFile {
    pub protocol_version: u8,
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub last_ready_build: Option<BuildIdentity>,
    pub selected_port: u16,
    pub launch_at_login: bool,
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub pending_operation: Option<PendingOperation>,
    pub legacy_migration_complete: bool,
}

impl Default for DesktopStateFile {
    fn default() -> Self {
        Self {
            protocol_version: PROTOCOL_VERSION,
            last_ready_build: None,
            selected_port: 8000,
            launch_at_login: false,
            pending_operation: None,
            legacy_migration_complete: false,
        }
    }
}

#[derive(Clone)]
pub struct DesktopStateStore {
    path: Arc<PathBuf>,
    value: Arc<Mutex<DesktopStateFile>>,
}

impl DesktopStateStore {
    pub fn load(path: PathBuf) -> Result<Self, String> {
        let value = load_file(&path)?;
        Ok(Self {
            path: Arc::new(path),
            value: Arc::new(Mutex::new(value)),
        })
    }

    pub fn snapshot(&self) -> DesktopStateFile {
        self.value.lock().expect("desktop state poisoned").clone()
    }

    pub fn replace(&self, next: DesktopStateFile) -> Result<(), String> {
        validate(&next)?;
        let bytes = serde_json::to_vec_pretty(&next)
            .map_err(|_| "The desktop state cannot be encoded.".to_owned())?;
        atomic_write_private(&self.path, &bytes)?;
        *self.value.lock().expect("desktop state poisoned") = next;
        Ok(())
    }

    pub fn update(
        &self,
        change: impl FnOnce(&mut DesktopStateFile) -> Result<(), String>,
    ) -> Result<DesktopStateFile, String> {
        let mut next = self.snapshot();
        change(&mut next)?;
        self.replace(next.clone())?;
        Ok(next)
    }

    pub fn persist(&self) -> Result<(), String> {
        self.replace(self.snapshot())
    }
}

fn load_file(path: &Path) -> Result<DesktopStateFile, String> {
    let metadata = match fs::symlink_metadata(path) {
        Ok(metadata) => metadata,
        Err(error) if error.kind() == std::io::ErrorKind::NotFound => {
            return Ok(DesktopStateFile::default());
        }
        Err(_) => return Err("The desktop state cannot be inspected.".to_owned()),
    };
    if metadata.file_type().is_symlink() || !metadata.is_file() || metadata.len() > STATE_LIMIT {
        return Err("The desktop state file has an unsafe file type or size.".to_owned());
    }
    #[cfg(unix)]
    {
        use std::os::unix::fs::PermissionsExt;
        if metadata.permissions().mode() & 0o077 != 0 {
            return Err("The desktop state file is not private.".to_owned());
        }
    }
    let bytes = fs::read(path).map_err(|_| "The desktop state cannot be read.".to_owned())?;
    let value: DesktopStateFile = serde_json::from_slice(&bytes)
        .map_err(|_| "The desktop state file is invalid.".to_owned())?;
    validate(&value)?;
    Ok(value)
}

fn validate(value: &DesktopStateFile) -> Result<(), String> {
    if value.protocol_version != PROTOCOL_VERSION {
        return Err("The desktop state protocol version is unsupported.".to_owned());
    }
    if value.selected_port == 0 {
        return Err("The desktop state contains an invalid local port.".to_owned());
    }
    if let Some(pending) = &value.pending_operation
        && (pending.backup_path.as_os_str().is_empty()
            || pending.backup_sha256.len() != 64
            || !pending
                .backup_sha256
                .bytes()
                .all(|byte| byte.is_ascii_hexdigit())
            || matches!(
                pending.kind,
                OperationKind::Restore | OperationKind::DowngradeRestore
            ) && pending
                .restore_source
                .as_ref()
                .is_none_or(|path| path.as_os_str().is_empty())
            || pending.restore_id.as_ref().is_some_and(String::is_empty))
    {
        return Err("The pending desktop operation is invalid.".to_owned());
    }
    Ok(())
}

#[cfg(test)]
mod tests {
    use super::*;
    use rand::RngCore;

    #[test]
    fn state_round_trip_keeps_one_pending_operation() {
        let mut suffix = [0_u8; 12];
        rand::rng().fill_bytes(&mut suffix);
        let root =
            std::env::temp_dir().join(format!("suchi-desktop-state-{}", hex::encode(suffix)));
        fs::create_dir(&root).unwrap();
        let path = root.join("desktop-state.json");
        let store = DesktopStateStore::load(path.clone()).unwrap();
        store
            .update(|state| {
                state.pending_operation = Some(PendingOperation {
                    kind: OperationKind::Upgrade,
                    source_build: BuildIdentity {
                        version: "v0.2.0".to_owned(),
                        revision: "old".to_owned(),
                    },
                    target_build: BuildIdentity {
                        version: "v0.3.0".to_owned(),
                        revision: "new".to_owned(),
                    },
                    backup_path: root.join("pre-upgrade.suchi-backup"),
                    backup_sha256: "a".repeat(64),
                    restore_id: None,
                    restore_source: None,
                });
                Ok(())
            })
            .unwrap();
        let loaded = DesktopStateStore::load(path).unwrap().snapshot();
        assert_eq!(
            loaded.pending_operation.unwrap().kind,
            OperationKind::Upgrade
        );
        fs::remove_dir_all(root).unwrap();
    }

    #[test]
    fn restore_journal_requires_selected_source_before_activation() {
        let build = BuildIdentity {
            version: "v0.3.0".to_owned(),
            revision: "current".to_owned(),
        };
        let mut state = DesktopStateFile {
            pending_operation: Some(PendingOperation {
                kind: OperationKind::Restore,
                source_build: build.clone(),
                target_build: build,
                backup_path: PathBuf::from("/tmp/retained.suchi-backup"),
                backup_sha256: "b".repeat(64),
                restore_source: None,
                restore_id: None,
            }),
            ..DesktopStateFile::default()
        };
        assert!(validate(&state).is_err());
        state
            .pending_operation
            .as_mut()
            .expect("pending restore")
            .restore_source = Some(PathBuf::from("/tmp/selected.suchi-backup"));
        assert!(validate(&state).is_ok());
    }
}
