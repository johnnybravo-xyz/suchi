// SPDX-License-Identifier: AGPL-3.0-or-later

use crate::identity::SidecarIdentity;
use serde::Deserialize;
use std::{
    fs,
    io::{Read, Write},
    net::{SocketAddr, TcpStream},
    path::Path,
    thread,
    time::{Duration, Instant},
};

const READY_LIMIT: u64 = 16 * 1024;

#[derive(Debug, Deserialize)]
#[serde(deny_unknown_fields)]
struct ReadyRecord {
    version: u8,
    nonce: String,
    pid: u32,
    public_url: String,
    build_version: String,
    build_revision: String,
}

pub fn wait_for_ready(
    path: &Path,
    nonce: &str,
    pid: u32,
    identity: &SidecarIdentity,
    port: u16,
    timeout: Duration,
) -> Result<(), String> {
    let origin = format!("http://127.0.0.1:{port}");
    let deadline = Instant::now() + timeout;
    while Instant::now() < deadline {
        match read_record(path) {
            Ok(record) => {
                if record.version != 1
                    || record.nonce != nonce
                    || record.pid != pid
                    || record.public_url != origin
                    || record.build_version != identity.version
                    || record.build_revision != identity.revision
                {
                    return Err("The local server returned invalid readiness proof.".to_owned());
                }
                return verify_http_ready(port);
            }
            Err(ReadinessRead::NotReady) => thread::sleep(Duration::from_millis(40)),
            Err(ReadinessRead::Invalid) => {
                return Err("The local server readiness file is invalid.".to_owned());
            }
        }
    }
    Err("The local server did not become ready in time.".to_owned())
}

enum ReadinessRead {
    NotReady,
    Invalid,
}

fn read_record(path: &Path) -> Result<ReadyRecord, ReadinessRead> {
    let metadata = match fs::symlink_metadata(path) {
        Ok(metadata) => metadata,
        Err(error) if error.kind() == std::io::ErrorKind::NotFound => {
            return Err(ReadinessRead::NotReady);
        }
        Err(_) => return Err(ReadinessRead::Invalid),
    };
    if metadata.file_type().is_symlink()
        || !metadata.is_file()
        || metadata.len() == 0
        || metadata.len() > READY_LIMIT
    {
        return Err(ReadinessRead::Invalid);
    }
    let bytes = fs::read(path).map_err(|_| ReadinessRead::Invalid)?;
    serde_json::from_slice(&bytes).map_err(|_| ReadinessRead::Invalid)
}

fn verify_http_ready(port: u16) -> Result<(), String> {
    let address = SocketAddr::from(([127, 0, 0, 1], port));
    let mut stream = TcpStream::connect_timeout(&address, Duration::from_secs(2))
        .map_err(|_| "The local server readiness endpoint is unavailable.".to_owned())?;
    stream
        .set_read_timeout(Some(Duration::from_secs(2)))
        .map_err(|_| "The local server readiness check could not set a timeout.".to_owned())?;
    stream
        .set_write_timeout(Some(Duration::from_secs(2)))
        .map_err(|_| "The local server readiness check could not set a timeout.".to_owned())?;
    write!(
        stream,
        "GET /readyz HTTP/1.1\r\nHost: 127.0.0.1:{port}\r\nConnection: close\r\n\r\n"
    )
    .map_err(|_| "The local server readiness request failed.".to_owned())?;
    let mut response = [0_u8; 1024];
    let count = stream
        .read(&mut response)
        .map_err(|_| "The local server readiness response failed.".to_owned())?;
    let first_line = response[..count]
        .split(|byte| *byte == b'\n')
        .next()
        .unwrap_or_default();
    if first_line.starts_with(b"HTTP/1.1 200 ") || first_line.starts_with(b"HTTP/1.0 200 ") {
        Ok(())
    } else {
        Err("The local server readiness endpoint rejected the request.".to_owned())
    }
}
