// SPDX-License-Identifier: AGPL-3.0-or-later

use serde::{Deserialize, Serialize};
use sha2::{Digest, Sha256};
use std::{
    fs::{self, File},
    io::Read,
    path::{Path, PathBuf},
};

const EMBEDDED_IDENTITY: &str = include_str!(concat!(env!("OUT_DIR"), "/sidecar-identity.json"));

#[derive(Clone, Debug, Deserialize, Serialize)]
#[serde(deny_unknown_fields)]
pub struct SidecarIdentity {
    pub schema_version: u8,
    pub version: String,
    pub revision: String,
    pub target: String,
    pub executable_type: String,
    pub filename: String,
    pub sha256: String,
}

impl SidecarIdentity {
    pub fn embedded() -> Result<Self, String> {
        let identity: Self = serde_json::from_str(EMBEDDED_IDENTITY)
            .map_err(|_| "The bundled server identity is invalid.".to_owned())?;
        identity.validate()?;
        Ok(identity)
    }

    fn validate(&self) -> Result<(), String> {
        if self.schema_version != 1
            || self.target != env!("SUCHI_DESKTOP_TARGET")
            || self.executable_type != "suchi-sidecar"
            || self.version.trim().is_empty()
            || self.revision.trim().is_empty()
            || self.filename != platform_filename()
            || self.sha256.len() != 64
            || !self.sha256.bytes().all(|byte| byte.is_ascii_hexdigit())
        {
            return Err("The bundled server identity does not match this application.".to_owned());
        }
        Ok(())
    }

    pub fn locate_and_verify(&self) -> Result<PathBuf, String> {
        let mut candidates = Vec::with_capacity(2);
        if let Ok(executable) = std::env::current_exe()
            && let Some(parent) = executable.parent()
        {
            candidates.push(parent.join(&self.filename));
        }
        candidates.push(
            Path::new(env!("CARGO_MANIFEST_DIR"))
                .join("binaries")
                .join(format!(
                    "suchi-sidecar-{}{}",
                    self.target,
                    executable_suffix()
                )),
        );

        let path = candidates
            .into_iter()
            .find(|candidate| candidate.is_file())
            .ok_or_else(|| "The bundled server executable is missing.".to_owned())?;
        let metadata = fs::symlink_metadata(&path)
            .map_err(|_| "The bundled server executable cannot be inspected.".to_owned())?;
        if metadata.file_type().is_symlink() || !metadata.is_file() {
            return Err("The bundled server executable has an unsafe file type.".to_owned());
        }

        let actual = digest_file(&path)?;
        if !actual.eq_ignore_ascii_case(&self.sha256) {
            return Err("The bundled server failed its integrity check.".to_owned());
        }
        Ok(path)
    }
}

fn digest_file(path: &Path) -> Result<String, String> {
    let mut file = File::open(path)
        .map_err(|_| "The bundled server executable cannot be opened.".to_owned())?;
    let mut digest = Sha256::new();
    let mut buffer = [0_u8; 64 * 1024];
    loop {
        let count = file
            .read(&mut buffer)
            .map_err(|_| "The bundled server executable cannot be read.".to_owned())?;
        if count == 0 {
            break;
        }
        digest.update(&buffer[..count]);
    }
    Ok(hex::encode(digest.finalize()))
}

fn platform_filename() -> String {
    format!("suchi-sidecar{}", executable_suffix())
}

fn executable_suffix() -> &'static str {
    if cfg!(windows) { ".exe" } else { "" }
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn embedded_identity_is_well_formed() {
        let identity = SidecarIdentity::embedded().expect("embedded identity");
        assert_eq!(identity.target, env!("SUCHI_DESKTOP_TARGET"));
    }
}
