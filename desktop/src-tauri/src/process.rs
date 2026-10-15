// SPDX-License-Identifier: AGPL-3.0-or-later

use crate::{
    environment::sidecar_environment, identity::SidecarIdentity, logging::RotatingLog, navigation,
    paths::ManagedPaths, prerequisites, readiness,
};
use rand::RngCore;
use serde::Serialize;
use std::{
    io::Read,
    net::TcpListener,
    process::{Child, ChildStdin, Command, ExitStatus, Stdio},
    sync::{
        Arc, Mutex,
        atomic::{AtomicBool, Ordering},
        mpsc,
    },
    thread,
    time::{Duration, Instant},
};
use tauri::AppHandle;

const STARTUP_TIMEOUT: Duration = Duration::from_secs(30);
const SHUTDOWN_TIMEOUT: Duration = Duration::from_secs(20);

#[derive(Clone)]
pub struct DesktopState {
    shared: Arc<Shared>,
}

struct Shared {
    identity: SidecarIdentity,
    paths: ManagedPaths,
    log: Arc<RotatingLog>,
    process: Mutex<ProcessState>,
    quitting: AtomicBool,
}

struct ProcessState {
    phase: Phase,
    pid: Option<u32>,
    stdin: Option<ChildStdin>,
    control: Option<mpsc::Sender<Control>>,
    detail: Option<String>,
}

#[derive(Clone, Copy, Debug, Eq, PartialEq)]
enum Phase {
    Stopped,
    Starting,
    Running,
    SetupRequired,
    Stopping,
    Error,
}

enum Control {
    Stop,
    Kill,
}

#[derive(Clone, Debug, Serialize)]
#[serde(rename_all = "camelCase")]
pub struct LauncherState {
    title: String,
    message: String,
    detail: Option<String>,
    setup_required: bool,
    can_retry: bool,
    can_open: bool,
    can_stop: bool,
}

impl DesktopState {
    pub fn new(paths: ManagedPaths, identity: SidecarIdentity) -> Result<Self, String> {
        let log = Arc::new(RotatingLog::open(paths.sidecar_log())?);
        Ok(Self {
            shared: Arc::new(Shared {
                identity,
                paths,
                log,
                process: Mutex::new(ProcessState {
                    phase: Phase::Stopped,
                    pid: None,
                    stdin: None,
                    control: None,
                    detail: None,
                }),
                quitting: AtomicBool::new(false),
            }),
        })
    }

    pub fn paths(&self) -> &ManagedPaths {
        &self.shared.paths
    }

    pub fn launcher_state(&self) -> LauncherState {
        let process = self.shared.process.lock().expect("process state poisoned");
        match process.phase {
            Phase::Stopped => LauncherState {
                title: "Processing is stopped".to_owned(),
                message: "Your archive is closed. Start processing to use Suchi.".to_owned(),
                detail: process.detail.clone(),
                setup_required: false,
                can_retry: true,
                can_open: false,
                can_stop: false,
            },
            Phase::Starting => LauncherState {
                title: "Starting your archive".to_owned(),
                message: "Checking prerequisites and opening the local document service…"
                    .to_owned(),
                detail: process.detail.clone(),
                setup_required: false,
                can_retry: false,
                can_open: false,
                can_stop: true,
            },
            Phase::Running => LauncherState {
                title: "Your archive is ready".to_owned(),
                message: "Suchi is running only on this computer.".to_owned(),
                detail: process.detail.clone(),
                setup_required: false,
                can_retry: false,
                can_open: true,
                can_stop: true,
            },
            Phase::SetupRequired => LauncherState {
                title: "Finish local setup".to_owned(),
                message: "Create the first local account to open your archive.".to_owned(),
                detail: process.detail.clone(),
                setup_required: true,
                can_retry: false,
                can_open: false,
                can_stop: true,
            },
            Phase::Stopping => LauncherState {
                title: "Stopping processing".to_owned(),
                message: "Suchi is closing the archive cleanly…".to_owned(),
                detail: None,
                setup_required: false,
                can_retry: false,
                can_open: false,
                can_stop: false,
            },
            Phase::Error => LauncherState {
                title: "Suchi could not start".to_owned(),
                message: "The local document service is unavailable.".to_owned(),
                detail: process.detail.clone(),
                setup_required: false,
                can_retry: true,
                can_open: false,
                can_stop: false,
            },
        }
    }

    pub fn start(&self, app: &AppHandle) -> Result<(), String> {
        {
            let mut process = self.shared.process.lock().expect("process state poisoned");
            match process.phase {
                Phase::Stopped | Phase::Error => {
                    process.phase = Phase::Starting;
                    process.detail = None;
                }
                Phase::Starting | Phase::Running | Phase::SetupRequired => return Ok(()),
                Phase::Stopping => return Err("Processing is still stopping.".to_owned()),
            }
        }
        navigation::navigate_to_launcher(app);

        if let Err(message) = ensure_fixed_port_available() {
            self.set_error(message.clone());
            return Err(message);
        }
        let sidecar = match self.shared.identity.locate_and_verify() {
            Ok(path) => path,
            Err(message) => {
                self.set_error(message.clone());
                return Err(message);
            }
        };
        match prerequisites::inspect(&sidecar, &self.shared.paths) {
            Ok(detail) => {
                self.shared
                    .process
                    .lock()
                    .expect("process state poisoned")
                    .detail = detail;
            }
            Err(message) => {
                self.set_error(message.clone());
                return Err(message);
            }
        }
        let nonce = random_nonce();
        let ready_file = self.shared.paths.ready_file(&nonce);
        let _ = std::fs::remove_file(&ready_file);

        let mut command = Command::new(sidecar);
        command
            .arg("serve")
            .arg("--setup-token-file")
            .arg(self.shared.paths.setup_token_file())
            .arg("--ready-file")
            .arg(&ready_file)
            .arg("--ready-nonce")
            .arg(&nonce)
            .arg("--exit-on-stdin-close")
            .env_clear()
            .envs(sidecar_environment(&self.shared.paths))
            .stdin(Stdio::piped())
            .stdout(Stdio::piped())
            .stderr(Stdio::piped());
        configure_process_group(&mut command);

        let mut child = match command.spawn() {
            Ok(child) => child,
            Err(_) => {
                let message = "The bundled local server could not be started.".to_owned();
                self.set_error(message.clone());
                return Err(message);
            }
        };
        let pid = child.id();
        let stdin = child.stdin.take();
        let stdout = child.stdout.take();
        let stderr = child.stderr.take();
        let (control, controls) = mpsc::channel();
        {
            let mut process = self.shared.process.lock().expect("process state poisoned");
            if process.phase != Phase::Starting {
                let _ = child.kill();
                return Err("Processing start was cancelled.".to_owned());
            }
            process.pid = Some(pid);
            process.stdin = stdin;
            process.control = Some(control);
        }

        if let Some(stdout) = stdout {
            drain_stream(stdout, Arc::clone(&self.shared.log), "stdout");
        }
        if let Some(stderr) = stderr {
            drain_stream(stderr, Arc::clone(&self.shared.log), "stderr");
        }
        monitor_process(child, controls, self.clone(), app.clone());

        let state = self.clone();
        let ready_app = app.clone();
        thread::spawn(move || {
            match readiness::wait_for_ready(
                &ready_file,
                &nonce,
                pid,
                &state.shared.identity,
                STARTUP_TIMEOUT,
            ) {
                Ok(()) => state.mark_ready(pid, &ready_app),
                Err(message) => state.fail_start(pid, message, &ready_app),
            }
        });
        Ok(())
    }

    pub fn stop(&self) {
        let control = {
            let mut process = self.shared.process.lock().expect("process state poisoned");
            if !matches!(
                process.phase,
                Phase::Starting | Phase::Running | Phase::SetupRequired
            ) {
                return;
            }
            process.phase = Phase::Stopping;
            process.stdin.take();
            process.control.clone()
        };
        if let Some(control) = control {
            let _ = control.send(Control::Stop);
        }
    }

    pub fn kill(&self) {
        let control = {
            let mut process = self.shared.process.lock().expect("process state poisoned");
            process.stdin.take();
            process.control.clone()
        };
        if let Some(control) = control {
            let _ = control.send(Control::Kill);
        }
    }

    pub fn begin_quit(&self, app: AppHandle) {
        if self.shared.quitting.swap(true, Ordering::SeqCst) {
            return;
        }
        self.stop();
        let state = self.clone();
        thread::spawn(move || {
            let deadline = Instant::now() + SHUTDOWN_TIMEOUT + Duration::from_secs(2);
            while Instant::now() < deadline {
                if state
                    .shared
                    .process
                    .lock()
                    .expect("process state poisoned")
                    .pid
                    .is_none()
                {
                    app.exit(0);
                    return;
                }
                thread::sleep(Duration::from_millis(50));
            }
            state.kill();
            app.exit(1);
        });
    }

    pub fn is_quitting(&self) -> bool {
        self.shared.quitting.load(Ordering::SeqCst)
    }

    pub fn reveal_setup_token(&self) -> Result<String, String> {
        let path = self.shared.paths.setup_token_file();
        let metadata = std::fs::symlink_metadata(&path)
            .map_err(|_| "The one-time setup token is not available.".to_owned())?;
        if metadata.file_type().is_symlink() || !metadata.is_file() || metadata.len() > 4096 {
            return Err("The one-time setup token file is unsafe.".to_owned());
        }
        #[cfg(unix)]
        {
            use std::os::unix::fs::PermissionsExt;
            if metadata.permissions().mode() & 0o077 != 0 {
                return Err("The one-time setup token file is not private.".to_owned());
            }
        }
        let token = std::fs::read_to_string(path)
            .map_err(|_| "The one-time setup token cannot be read.".to_owned())?;
        let token = token.trim();
        if token.is_empty() {
            return Err("The one-time setup token is empty.".to_owned());
        }
        Ok(token.to_owned())
    }

    fn mark_ready(&self, pid: u32, app: &AppHandle) {
        let setup_required = self.shared.paths.setup_token_file().is_file();
        let mut process = self.shared.process.lock().expect("process state poisoned");
        if process.pid != Some(pid) || process.phase != Phase::Starting {
            return;
        }
        process.phase = if setup_required {
            Phase::SetupRequired
        } else {
            Phase::Running
        };
        drop(process);
        if setup_required {
            navigation::navigate_to_launcher(app);
        } else {
            navigation::navigate_to_application(app);
        }
    }

    fn fail_start(&self, pid: u32, message: String, app: &AppHandle) {
        let control = {
            let mut process = self.shared.process.lock().expect("process state poisoned");
            if process.pid != Some(pid) || process.phase != Phase::Starting {
                return;
            }
            process.phase = Phase::Error;
            process.detail = Some(message);
            process.stdin.take();
            process.control.clone()
        };
        if let Some(control) = control {
            let _ = control.send(Control::Stop);
        }
        navigation::navigate_to_launcher(app);
    }

    fn set_error(&self, message: String) {
        let mut process = self.shared.process.lock().expect("process state poisoned");
        process.phase = Phase::Error;
        process.detail = Some(message);
        process.pid = None;
        process.stdin = None;
        process.control = None;
    }

    fn exited(&self, pid: u32, status: std::io::Result<ExitStatus>, app: &AppHandle) {
        let mut process = self.shared.process.lock().expect("process state poisoned");
        if process.pid != Some(pid) {
            return;
        }
        process.pid = None;
        process.stdin = None;
        process.control = None;
        match process.phase {
            Phase::Stopping => {
                process.phase = Phase::Stopped;
                process.detail = None;
            }
            Phase::Error => {}
            _ => {
                process.phase = Phase::Error;
                process.detail = Some(match status {
                    Ok(exit) if exit.success() => {
                        "The local server stopped unexpectedly. You can retry safely.".to_owned()
                    }
                    _ => {
                        "The local server exited unexpectedly. Review the private log, then retry."
                            .to_owned()
                    }
                });
            }
        }
        let show_launcher = process.phase == Phase::Error;
        drop(process);
        if show_launcher && !self.is_quitting() {
            navigation::navigate_to_launcher(app);
        }
    }
}

fn ensure_fixed_port_available() -> Result<(), String> {
    TcpListener::bind(("127.0.0.1", 8000))
        .map(drop)
        .map_err(|_| "Port 8000 is already in use. Stop the other service and retry; Suchi will not choose a different origin.".to_owned())
}

fn random_nonce() -> String {
    let mut bytes = [0_u8; 24];
    rand::rng().fill_bytes(&mut bytes);
    hex::encode(bytes)
}

fn drain_stream<R: Read + Send + 'static>(
    mut stream: R,
    log: Arc<RotatingLog>,
    name: &'static str,
) {
    thread::spawn(move || {
        let mut buffer = [0_u8; 8192];
        loop {
            match stream.read(&mut buffer) {
                Ok(0) => return,
                Ok(count) => log.write(name, &buffer[..count]),
                Err(_) => return,
            }
        }
    });
}

fn monitor_process(
    mut child: Child,
    controls: mpsc::Receiver<Control>,
    state: DesktopState,
    app: AppHandle,
) {
    let pid = child.id();
    thread::spawn(move || {
        let mut stop_deadline = None;
        loop {
            match controls.try_recv() {
                Ok(Control::Stop) => stop_deadline = Some(Instant::now() + SHUTDOWN_TIMEOUT),
                Ok(Control::Kill) => {
                    kill_process_group(pid);
                    let _ = child.kill();
                }
                Err(mpsc::TryRecvError::Disconnected) => {}
                Err(mpsc::TryRecvError::Empty) => {}
            }
            match child.try_wait() {
                Ok(Some(status)) => {
                    state.exited(pid, Ok(status), &app);
                    return;
                }
                Ok(None) => {}
                Err(error) => {
                    state.exited(pid, Err(error), &app);
                    return;
                }
            }
            if stop_deadline.is_some_and(|deadline| Instant::now() >= deadline) {
                kill_process_group(pid);
                let _ = child.kill();
                stop_deadline = None;
            }
            thread::sleep(Duration::from_millis(50));
        }
    });
}

#[cfg(unix)]
fn configure_process_group(command: &mut Command) {
    use std::os::unix::process::CommandExt;
    command.process_group(0);
}

#[cfg(windows)]
fn configure_process_group(command: &mut Command) {
    use std::os::windows::process::CommandExt;
    const CREATE_NEW_PROCESS_GROUP: u32 = 0x0000_0200;
    command.creation_flags(CREATE_NEW_PROCESS_GROUP);
}

#[cfg(unix)]
fn kill_process_group(pid: u32) {
    // SAFETY: the sidecar is spawned as the leader of a new process group.
    unsafe {
        libc::kill(-(pid as i32), libc::SIGKILL);
    }
}

#[cfg(windows)]
fn kill_process_group(_pid: u32) {}
