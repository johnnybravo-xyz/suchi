// SPDX-License-Identifier: AGPL-3.0-or-later

use crate::{paths::ManagedPaths, sidecar_cli};
use serde::Deserialize;
use std::{ffi::OsString, path::Path};

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

pub fn inspect(
    executable: &Path,
    paths: &ManagedPaths,
    port: u16,
) -> Result<Option<String>, String> {
    let arguments = [OsString::from("prerequisites"), OsString::from("--json")];
    let output =
        sidecar_cli::run_json::<Report>(executable, paths, port, &arguments, "prerequisite check")?;
    if !output.success {
        return Err("The prerequisite check failed.".to_owned());
    }
    let report = output.value;
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
