// SPDX-License-Identifier: AGPL-3.0-or-later

use crate::{environment::sidecar_environment, paths::ManagedPaths};
use serde::de::DeserializeOwned;
use std::{
    ffi::OsString,
    io::Read,
    path::Path,
    process::{Command, Stdio},
    thread,
};

const OUTPUT_LIMIT: usize = 1024 * 1024;

pub struct JsonCommand<T> {
    pub value: T,
    pub success: bool,
}

pub fn run_json<T: DeserializeOwned>(
    executable: &Path,
    paths: &ManagedPaths,
    port: u16,
    arguments: &[OsString],
    label: &str,
) -> Result<JsonCommand<T>, String> {
    let mut command = Command::new(executable);
    command
        .args(arguments)
        .env_clear()
        .envs(sidecar_environment(paths, port))
        .stdin(Stdio::null())
        .stdout(Stdio::piped())
        .stderr(Stdio::piped());
    let mut child = command
        .spawn()
        .map_err(|_| format!("The {label} command could not start."))?;
    let stdout = child.stdout.take().expect("piped stdout");
    let stderr = child.stderr.take().expect("piped stderr");
    let stdout = thread::spawn(move || read_bounded(stdout));
    let stderr = thread::spawn(move || read_bounded(stderr));
    let status = child
        .wait()
        .map_err(|_| format!("The {label} command could not finish."))?;
    let stdout = stdout
        .join()
        .map_err(|_| format!("The {label} output reader failed."))??;
    let _stderr = stderr
        .join()
        .map_err(|_| format!("The {label} error reader failed."))??;
    let value = serde_json::from_slice(&stdout)
        .map_err(|_| format!("The {label} command returned an invalid report."))?;
    Ok(JsonCommand {
        value,
        success: status.success(),
    })
}

fn read_bounded<R: Read>(mut reader: R) -> Result<Vec<u8>, String> {
    let mut output = Vec::new();
    let mut buffer = [0_u8; 8192];
    loop {
        let count = reader
            .read(&mut buffer)
            .map_err(|_| "A sidecar command output stream could not be read.".to_owned())?;
        if count == 0 {
            return Ok(output);
        }
        if output.len() + count > OUTPUT_LIMIT {
            return Err("A sidecar command exceeded its output safety limit.".to_owned());
        }
        output.extend_from_slice(&buffer[..count]);
    }
}
