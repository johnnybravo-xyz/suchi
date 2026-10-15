// SPDX-License-Identifier: AGPL-3.0-or-later

use crate::{environment::sidecar_environment, paths::ManagedPaths};
use serde::Deserialize;
use std::{
    io::{Read, Write},
    path::Path,
    process::{Command, Stdio},
    thread,
};

const OUTPUT_LIMIT: usize = 1024 * 1024;

#[derive(Debug, Deserialize)]
struct Report {
    version: u8,
    degraded: bool,
    tools: Vec<Tool>,
    english_tesseract_data: Capability,
    image_to_pdf_conversion: Capability,
}

#[derive(Debug, Deserialize)]
struct Tool {
    name: String,
    available: bool,
}

#[derive(Debug, Deserialize)]
struct Capability {
    available: bool,
}

pub fn inspect(executable: &Path, paths: &ManagedPaths) -> Result<Option<String>, String> {
    let mut command = Command::new(executable);
    command
        .args(["prerequisites", "--json"])
        .env_clear()
        .envs(sidecar_environment(paths))
        .stdin(Stdio::null())
        .stdout(Stdio::piped())
        .stderr(Stdio::piped());
    let mut child = command
        .spawn()
        .map_err(|_| "The prerequisite check could not start.".to_owned())?;
    let stdout = child.stdout.take().expect("piped stdout");
    let stderr = child.stderr.take().expect("piped stderr");
    let stdout = thread::spawn(move || read_bounded(stdout));
    let stderr = thread::spawn(move || read_bounded(stderr));
    let status = child
        .wait()
        .map_err(|_| "The prerequisite check could not finish.".to_owned())?;
    let stdout = stdout
        .join()
        .map_err(|_| "The prerequisite output reader failed.".to_owned())??;
    let stderr = stderr
        .join()
        .map_err(|_| "The prerequisite error reader failed.".to_owned())??;
    if !status.success() {
        let message = String::from_utf8_lossy(&stderr);
        return Err(if message.trim().is_empty() {
            "The prerequisite check failed.".to_owned()
        } else {
            "The prerequisite check reported an internal error.".to_owned()
        });
    }
    let report: Report = serde_json::from_slice(&stdout)
        .map_err(|_| "The prerequisite report is invalid.".to_owned())?;
    if report.version != 1 {
        return Err("The prerequisite report version is unsupported.".to_owned());
    }
    if !report.degraded {
        return Ok(None);
    }
    let missing = report
        .tools
        .iter()
        .filter(|tool| !tool.available)
        .map(|tool| tool.name.as_str())
        .collect::<Vec<_>>();
    let mut guidance = Vec::new();
    if !missing.is_empty() {
        guidance.push(format!(
            "Optional parsers unavailable: {}.",
            missing.join(", ")
        ));
    }
    if !report.english_tesseract_data.available {
        guidance.push("English OCR language data is unavailable.".to_owned());
    }
    if !report.image_to_pdf_conversion.available {
        guidance.push("Image-to-PDF conversion is unavailable.".to_owned());
    }
    guidance.push("Suchi will keep working for supported document types. Install the missing local tools, then retry to enable those formats.".to_owned());
    Ok(Some(guidance.join(" ")))
}

fn read_bounded<R: Read>(mut reader: R) -> Result<Vec<u8>, String> {
    let mut output = Vec::new();
    let mut buffer = [0_u8; 8192];
    loop {
        let count = reader
            .read(&mut buffer)
            .map_err(|_| "The prerequisite output could not be read.".to_owned())?;
        if count == 0 {
            return Ok(output);
        }
        if output.len() + count > OUTPUT_LIMIT {
            return Err("The prerequisite output exceeded its safety limit.".to_owned());
        }
        output.write_all(&buffer[..count]).expect("write to Vec");
    }
}
