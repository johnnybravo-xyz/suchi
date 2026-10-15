// SPDX-License-Identifier: AGPL-3.0-or-later

use std::{
    fs::{self, File, OpenOptions},
    io::{self, Write},
    path::{Path, PathBuf},
    sync::Mutex,
};

const MAX_LOG_BYTES: u64 = 1024 * 1024;
const RETAINED_LOGS: usize = 3;

pub struct RotatingLog {
    path: PathBuf,
    file: Mutex<File>,
}

impl RotatingLog {
    pub fn open(path: PathBuf) -> Result<Self, String> {
        reject_unsafe_existing(&path)?;
        let file =
            open_append(&path).map_err(|_| "The local server log cannot be opened.".to_owned())?;
        Ok(Self {
            path,
            file: Mutex::new(file),
        })
    }

    pub fn write(&self, stream: &str, bytes: &[u8]) {
        let Ok(mut file) = self.file.lock() else {
            return;
        };
        if file
            .metadata()
            .is_ok_and(|metadata| metadata.len() >= MAX_LOG_BYTES)
        {
            let _ = file.flush();
            if rotate(&self.path).is_ok()
                && let Ok(replacement) = open_append(&self.path)
            {
                *file = replacement;
            }
        }
        let _ = write!(file, "[{stream}] ");
        let _ = file.write_all(bytes);
        if !bytes.ends_with(b"\n") {
            let _ = file.write_all(b"\n");
        }
        let _ = file.flush();
    }
}

fn rotate(path: &Path) -> io::Result<()> {
    for index in (1..RETAINED_LOGS).rev() {
        let source = numbered(path, index);
        let destination = numbered(path, index + 1);
        if source.exists() {
            let _ = fs::remove_file(&destination);
            fs::rename(source, destination)?;
        }
    }
    if path.exists() {
        let destination = numbered(path, 1);
        let _ = fs::remove_file(&destination);
        fs::rename(path, destination)?;
    }
    Ok(())
}

fn numbered(path: &Path, index: usize) -> PathBuf {
    PathBuf::from(format!("{}.{}", path.display(), index))
}

fn reject_unsafe_existing(path: &Path) -> Result<(), String> {
    match fs::symlink_metadata(path) {
        Ok(metadata) if metadata.file_type().is_symlink() || !metadata.is_file() => {
            Err("The local server log has an unsafe file type.".to_owned())
        }
        Ok(_) => Ok(()),
        Err(error) if error.kind() == io::ErrorKind::NotFound => Ok(()),
        Err(_) => Err("The local server log cannot be inspected.".to_owned()),
    }
}

fn open_append(path: &Path) -> io::Result<File> {
    let mut options = OpenOptions::new();
    options.create(true).append(true);
    #[cfg(unix)]
    {
        use std::os::unix::fs::OpenOptionsExt;
        options.mode(0o600);
    }
    options.open(path)
}
