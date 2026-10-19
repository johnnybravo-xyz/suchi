// SPDX-License-Identifier: AGPL-3.0-or-later

use crate::{
    backup,
    desktop_state::{BuildIdentity, DesktopStateStore, OperationKind},
    environment::sidecar_environment,
    identity::SidecarIdentity,
    legacy::{self, LegacyInstall},
    logging::RotatingLog,
    navigation,
    paths::ManagedPaths,
    prerequisites, readiness, upgrade,
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
use tauri_plugin_autostart::ManagerExt as _;

const STARTUP_TIMEOUT: Duration = Duration::from_secs(30);
const SHUTDOWN_TIMEOUT: Duration = Duration::from_secs(20);

#[derive(Clone)]
pub struct DesktopState {
    shared: Arc<Shared>,
}

pub struct OperationGuard {
    state: DesktopState,
}

impl Drop for OperationGuard {
    fn drop(&mut self) {
        self.state.shared.operation.store(false, Ordering::SeqCst);
    }
}

struct Shared {
    identity: SidecarIdentity,
    paths: ManagedPaths,
    desktop_state: DesktopStateStore,
    port: u16,
    origin: String,
    log: Arc<RotatingLog>,
    legacy: Option<LegacyInstall>,
    startup_blocker: Option<String>,
    process: Mutex<ProcessState>,
    operation: AtomicBool,
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
    Maintenance,
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
    busy: bool,
    setup_required: bool,
    can_retry: bool,
    can_open: bool,
    can_stop: bool,
    can_rollback: bool,
    can_open_logs: bool,
    rollback_label: Option<String>,
}

impl DesktopState {
    pub fn new(
        paths: ManagedPaths,
        identity: SidecarIdentity,
        desktop_state: DesktopStateStore,
        legacy: Option<LegacyInstall>,
        startup_blocker: Option<String>,
    ) -> Result<Self, String> {
        let port = desktop_state.snapshot().selected_port;
        let origin = format!("http://127.0.0.1:{port}");
        let log = Arc::new(RotatingLog::open(paths.sidecar_log())?);
        Ok(Self {
            shared: Arc::new(Shared {
                identity,
                paths,
                desktop_state,
                port,
                origin,
                log,
                legacy,
                startup_blocker,
                process: Mutex::new(ProcessState {
                    phase: Phase::Stopped,
                    pid: None,
                    stdin: None,
                    control: None,
                    detail: None,
                }),
                operation: AtomicBool::new(false),
                quitting: AtomicBool::new(false),
            }),
        })
    }

    pub fn paths(&self) -> &ManagedPaths {
        &self.shared.paths
    }

    pub fn origin(&self) -> &str {
        &self.shared.origin
    }

    pub(crate) fn port(&self) -> u16 {
        self.shared.port
    }

    pub(crate) fn state_store(&self) -> &DesktopStateStore {
        &self.shared.desktop_state
    }

    pub(crate) fn sidecar_executable(&self) -> Result<std::path::PathBuf, String> {
        self.shared.identity.locate_and_verify()
    }

    pub(crate) fn build_identity(&self) -> BuildIdentity {
        BuildIdentity::from(&self.shared.identity)
    }

    pub(crate) fn set_launch_at_login_preference(&self, enabled: bool) -> Result<(), String> {
        self.shared.desktop_state.update(|state| {
            state.launch_at_login = enabled;
            Ok(())
        })?;
        Ok(())
    }

    pub(crate) fn begin_operation(&self) -> Result<OperationGuard, String> {
        self.shared
            .operation
            .compare_exchange(false, true, Ordering::SeqCst, Ordering::SeqCst)
            .map_err(|_| "Another archive operation is already running.".to_owned())?;
        Ok(OperationGuard {
            state: self.clone(),
        })
    }

    pub(crate) fn wait_until_stopped(&self, timeout: Duration) -> Result<(), String> {
        let deadline = Instant::now() + timeout;
        while Instant::now() < deadline {
            let process = self.shared.process.lock().expect("process state poisoned");
            if process.pid.is_none() && matches!(process.phase, Phase::Stopped | Phase::Error) {
                return Ok(());
            }
            drop(process);
            thread::sleep(Duration::from_millis(50));
        }
        Err("The local server did not stop in time.".to_owned())
    }

    pub(crate) fn set_maintenance(&self, detail: impl Into<String>) -> Result<(), String> {
        let mut process = self.shared.process.lock().expect("process state poisoned");
        if process.pid.is_some() {
            return Err("The local server is still running.".to_owned());
        }
        process.phase = Phase::Maintenance;
        process.detail = Some(detail.into());
        Ok(())
    }

    pub(crate) fn finish_maintenance(&self) {
        let mut process = self.shared.process.lock().expect("process state poisoned");
        if process.phase == Phase::Maintenance {
            process.phase = Phase::Stopped;
            process.detail = None;
        }
    }

    pub(crate) fn log_operation_error(&self, label: &str, message: &str) {
        self.shared.log.write(label, message.as_bytes());
    }
    pub fn launcher_state(&self) -> LauncherState {
        let process = self.shared.process.lock().expect("process state poisoned");
        let rollback_label =
            self.shared
                .desktop_state
                .snapshot()
                .pending_operation
                .map(|pending| match pending.kind {
                    OperationKind::Restore => "Roll back restore".to_owned(),
                    OperationKind::DowngradeRestore => "Roll back downgrade restore".to_owned(),
                    OperationKind::Upgrade | OperationKind::LegacyMigration => {
                        "Restore backup and quit".to_owned()
                    }
                });
        match process.phase {
            Phase::Stopped => LauncherState {
                title: "Processing is stopped".to_owned(),
                message: "Your archive is closed. Start processing to use Suchi.".to_owned(),
                detail: process.detail.clone(),
                busy: false,
                setup_required: false,
                can_retry: true,
                can_open: false,
                can_stop: false,
                can_rollback: false,
                can_open_logs: false,
                rollback_label: None,
            },
            Phase::Starting => LauncherState {
                title: "Starting your archive".to_owned(),
                message: "Checking prerequisites and opening the local document service…"
                    .to_owned(),
                detail: process.detail.clone(),
                busy: true,
                setup_required: false,
                can_retry: false,
                can_open: false,
                can_stop: true,
                can_rollback: false,
                can_open_logs: false,
                rollback_label: None,
            },
            Phase::Running => LauncherState {
                title: "Your archive is ready".to_owned(),
                message: "Suchi is running only on this computer.".to_owned(),
                detail: process.detail.clone(),
                busy: false,
                setup_required: false,
                can_retry: false,
                can_open: true,
                can_stop: true,
                can_rollback: false,
                can_open_logs: false,
                rollback_label: None,
            },
            Phase::SetupRequired => LauncherState {
                title: "Finish local setup".to_owned(),
                message: "Create the first local account to open your archive.".to_owned(),
                detail: process.detail.clone(),
                busy: false,
                setup_required: true,
                can_retry: false,
                can_open: false,
                can_stop: true,
                can_rollback: false,
                can_open_logs: false,
                rollback_label: None,
            },
            Phase::Stopping => LauncherState {
                title: "Stopping processing".to_owned(),
                message: "Suchi is closing the archive cleanly…".to_owned(),
                detail: None,
                busy: true,
                setup_required: false,
                can_retry: false,
                can_open: false,
                can_stop: false,
                can_rollback: false,
                can_open_logs: false,
                rollback_label: None,
            },
            Phase::Maintenance => LauncherState {
                title: "Protecting your archive".to_owned(),
                message: process
                    .detail
                    .clone()
                    .unwrap_or_else(|| "Suchi is completing a local archive operation…".to_owned()),
                detail: None,
                busy: true,
                setup_required: false,
                can_retry: false,
                can_open: false,
                can_stop: false,
                can_rollback: false,
                can_open_logs: false,
                rollback_label: None,
            },
            Phase::Error => LauncherState {
                title: "Suchi could not start".to_owned(),
                message: "Review the details and private logs, fix the cause, then retry."
                    .to_owned(),
                detail: process.detail.clone(),
                busy: false,
                setup_required: false,
                can_retry: true,
                can_open: false,
                can_stop: false,
                can_rollback: rollback_label.is_some(),
                can_open_logs: true,
                rollback_label,
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
                Phase::Maintenance => {
                    return Err("An archive operation is still running.".to_owned());
                }
            }
        }
        navigation::navigate_to_launcher(app);

        let sidecar = match self.shared.identity.locate_and_verify() {
            Ok(path) => path,
            Err(message) => {
                self.set_error(message.clone());
                return Err(message);
            }
        };
        if let Some(message) = self.shared.startup_blocker.clone() {
            self.set_error(message.clone());
            return Err(message);
        }
        match backup::resume_pending_restore(app, self, &sidecar) {
            Ok(true) => return Ok(()),
            Ok(false) => {}
            Err(message) => {
                self.set_error(message.clone());
                return Err(message);
            }
        }
        let prepared = if let Some(legacy) = &self.shared.legacy {
            legacy::prepare(app, self, &sidecar, legacy)
                .and_then(|()| self.shared.paths.write_managed_config(self.shared.port))
        } else {
            ensure_port_available(self.shared.port)
                .and_then(|()| upgrade::prepare(app, self, &sidecar))
                .and_then(|()| self.shared.paths.write_managed_config(self.shared.port))
        };
        if let Err(message) = prepared {
            self.set_error(message.clone());
            return Err(message);
        }
        if let Err(message) = ensure_port_available(self.shared.port) {
            self.set_error(message.clone());
            return Err(message);
        }
        match prerequisites::inspect(&sidecar, &self.shared.paths, self.shared.port) {
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
            .envs(sidecar_environment(&self.shared.paths, self.shared.port))
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
                state.shared.port,
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
        if let Some(pending) = self
            .shared
            .desktop_state
            .snapshot()
            .pending_operation
            .filter(|pending| pending.kind == OperationKind::Restore)
        {
            process.phase = Phase::Stopping;
            process.detail = Some("Validating the restored archive before commit…".to_owned());
            process.stdin.take();
            let control = process.control.clone();
            drop(process);
            if let Some(control) = control {
                let _ = control.send(Control::Stop);
            }
            backup::finalize_restore_after_ready(app.clone(), self.clone(), pending);
            return;
        }
        let pending_kind = self
            .shared
            .desktop_state
            .snapshot()
            .pending_operation
            .map(|pending| pending.kind);
        if pending_kind == Some(OperationKind::LegacyMigration) {
            let preference = self.shared.desktop_state.snapshot().launch_at_login;
            let login_result = if preference {
                app.autolaunch().enable()
            } else {
                app.autolaunch().disable()
            };
            if login_result.is_err() {
                process.phase = Phase::Error;
                process.detail = Some(
                    "The new launch-at-login setting could not be installed. Fix the application login-item permission, then retry or restore the migration backup."
                        .to_owned(),
                );
                process.stdin.take();
                let control = process.control.clone();
                drop(process);
                if let Some(control) = control {
                    let _ = control.send(Control::Stop);
                }
                navigation::navigate_to_launcher(app);
                return;
            }
        }
        let target = BuildIdentity::from(&self.shared.identity);
        if let Err(message) = self.shared.desktop_state.update(|state| {
            state.last_ready_build = Some(target);
            if state.pending_operation.as_ref().is_some_and(|pending| {
                matches!(
                    pending.kind,
                    OperationKind::Upgrade | OperationKind::LegacyMigration
                )
            }) {
                if state
                    .pending_operation
                    .as_ref()
                    .is_some_and(|pending| pending.kind == OperationKind::LegacyMigration)
                {
                    state.legacy_migration_complete = true;
                }
                state.pending_operation = None;
            }
            Ok(())
        }) {
            process.phase = Phase::Error;
            process.detail = Some(message);
            process.stdin.take();
            let control = process.control.clone();
            drop(process);
            if let Some(control) = control {
                let _ = control.send(Control::Stop);
            }
            navigation::navigate_to_launcher(app);
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
            navigation::navigate_to_application(app, &self.shared.origin);
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

    pub(crate) fn set_error(&self, message: String) {
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

fn ensure_port_available(port: u16) -> Result<(), String> {
    TcpListener::bind(("127.0.0.1", port))
        .map(drop)
        .map_err(|_| format!("Port {port} is already in use. Stop the other service and retry; Suchi will not choose a different origin."))
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
