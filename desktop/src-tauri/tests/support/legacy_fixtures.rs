// SPDX-License-Identifier: AGPL-3.0-or-later

use std::path::{Path, PathBuf};

pub const MANAGED_MARKER: &str = "Managed by Suchi local installer.\n";
pub const MANAGED_FILE_MARKER: &str = "# Managed by Suchi local installer.";
pub const MANAGED_XML_MARKER: &str = "<!-- Managed by Suchi local installer. -->";

#[derive(Debug)]
pub struct LegacyLinuxFixture {
    pub archive: PathBuf,
    pub config: PathBuf,
    pub program: PathBuf,
    pub marker: PathBuf,
    pub unit: PathBuf,
    pub desktop_entry: PathBuf,
    pub port_file: PathBuf,
    pub port: u16,
    pub login_enabled: bool,
    pub owned_program_files: Vec<PathBuf>,
    pub unit_contents: String,
    pub desktop_contents: String,
}

impl LegacyLinuxFixture {
    pub fn build(
        home: &Path,
        xdg_data: &Path,
        xdg_config: &Path,
        port: u16,
        login_enabled: bool,
    ) -> Self {
        let local_root = xdg_data.join("suchi-local");
        let program = local_root.join("program");
        let archive = local_root.join("archive");
        let config = xdg_config.join("suchi-local/local-install.toml");
        let unit = xdg_config.join("systemd/user/suchi-local.service");
        let desktop_entry = xdg_data.join("applications/suchi-local.desktop");
        let marker = program.join("managed-by-suchi-local");
        let port_file = program.join("port");
        let owned_program_files = [
            "suchi",
            "install.sh",
            "launch.sh",
            "suchi-tile.svg",
            "port",
            "open-with",
            "managed-by-suchi-local",
        ]
        .into_iter()
        .map(|name| program.join(name))
        .collect();
        let unit_contents = format!(
            "{MANAGED_FILE_MARKER}\n[Unit]\nDescription=Suchi local document archive\n\n[Service]\nType=notify\nNotifyAccess=main\nRuntimeDirectory=suchi-local\nRuntimeDirectoryMode=0700\nUMask=0077\nWorkingDirectory={}\nExecStart=/usr/bin/env -i \"HOME={}\" \"PATH=/usr/local/bin:/usr/bin:/bin\" \"NOTIFY_SOCKET=${{NOTIFY_SOCKET}}\" \"DATA_DIR={}\" \"SUCHI_CONFIG={}\" \"LISTEN_ADDR=127.0.0.1:{port}\" \"PUBLIC_URL=http://suchi.localhost:{port}\" \"SUCHI_DEV=0\" \"SUCHI_DEMO_MODE=0\" \"{}/suchi\" serve --systemd \"--setup-token-file=%t/suchi-local/setup-token\"\nRestart=on-failure\nRestartSec=5s\nTimeoutStartSec=120s\nTimeoutStopSec=60s\nNoNewPrivileges=true\n\n[Install]\nWantedBy=default.target\n",
            archive.display(),
            home.display(),
            archive.display(),
            config.display(),
            program.display(),
        );
        let desktop_contents = format!(
            "{MANAGED_FILE_MARKER}\n[Desktop Entry]\nType=Application\nName=Suchi\nComment=Open your local document archive\nExec=\"{}/launch.sh\"\nIcon={}/suchi-tile.svg\nTerminal=false\nCategories=Office;\nActions=Settings;Stop;\n\n[Desktop Action Settings]\nName=Settings\nExec=\"{}/launch.sh\" --settings\n\n[Desktop Action Stop]\nName=Stop Suchi\nExec=\"{}/launch.sh\" --stop\n",
            program.display(),
            program.display(),
            program.display(),
            program.display(),
        );
        Self {
            archive,
            config,
            program,
            marker,
            unit,
            desktop_entry,
            port_file,
            port,
            login_enabled,
            owned_program_files,
            unit_contents,
            desktop_contents,
        }
    }
}

#[derive(Debug)]
pub struct LegacyMacFixture {
    pub root: PathBuf,
    pub archive: PathBuf,
    pub config: PathBuf,
    pub runtime: PathBuf,
    pub program: PathBuf,
    pub marker: PathBuf,
    pub service_plist: PathBuf,
    pub login_plist: PathBuf,
    pub app: PathBuf,
    pub settings_app: PathBuf,
    pub port_file: PathBuf,
    pub login_preference_file: PathBuf,
    pub port: u16,
    pub login_enabled: bool,
    pub owned_program_files: Vec<PathBuf>,
    pub service_contents: String,
}

impl LegacyMacFixture {
    pub fn build(home: &Path, port: u16, login_enabled: bool) -> Self {
        let root = home.join("Library/Application Support/Suchi");
        let program = root.join("program");
        let archive = root.join("archive");
        let config = root.join("config/local-install.toml");
        let runtime = root.join("runtime");
        let marker = program.join("managed-by-suchi-local");
        let service_plist = program.join("page.suchi.local.plist");
        let login_plist = home.join("Library/LaunchAgents/page.suchi.local.plist");
        let app = home.join("Applications/Suchi.app");
        let settings_app = home.join("Applications/Suchi Settings.app");
        let port_file = program.join("port");
        let login_preference_file = program.join("start-at-login");
        let owned_program_files = [
            "suchi",
            "install.sh",
            "launch.sh",
            "common.sh",
            "settings.sh",
            "icon.png",
            "Suchi.icns",
            "page.suchi.local.plist",
            "port",
            "start-at-login",
            "managed-by-suchi-local",
        ]
        .into_iter()
        .map(|name| program.join(name))
        .collect();
        let service_contents = format!(
            "<?xml version=\"1.0\" encoding=\"UTF-8\"?>\n{MANAGED_XML_MARKER}\n<!DOCTYPE plist PUBLIC \"-//Apple//DTD PLIST 1.0//EN\" \"http://www.apple.com/DTDs/PropertyList-1.0.dtd\">\n<plist version=\"1.0\"><dict>\n<key>Label</key><string>page.suchi.local</string>\n<key>ProgramArguments</key><array>\n<string>/usr/bin/env</string><string>-i</string>\n<string>HOME={}</string>\n<string>PATH=/opt/homebrew/bin:/usr/local/bin:/usr/bin:/bin:/usr/sbin:/sbin</string>\n<string>DATA_DIR={}</string>\n<string>SUCHI_CONFIG={}</string>\n<string>LISTEN_ADDR=127.0.0.1:{port}</string>\n<string>PUBLIC_URL=http://127.0.0.1:{port}</string>\n<string>SUCHI_DEV=0</string><string>SUCHI_DEMO_MODE=0</string>\n<string>{}/suchi</string><string>serve</string>\n<string>--setup-token-file={}/setup-token</string>\n</array>\n<key>WorkingDirectory</key><string>{}</string>\n<key>KeepAlive</key><true/>\n<key>ThrottleInterval</key><integer>10</integer>\n<key>ExitTimeOut</key><integer>60</integer>\n<key>Umask</key><integer>63</integer>\n<key>StandardOutPath</key><string>{}/server.log</string>\n<key>StandardErrorPath</key><string>{}/server.log</string>\n</dict></plist>\n",
            home.display(),
            archive.display(),
            config.display(),
            program.display(),
            runtime.display(),
            archive.display(),
            runtime.display(),
            runtime.display(),
        );
        Self {
            root,
            archive,
            config,
            runtime,
            program,
            marker,
            service_plist,
            login_plist,
            app,
            settings_app,
            port_file,
            login_preference_file,
            port,
            login_enabled,
            owned_program_files,
            service_contents,
        }
    }

    pub fn owned_app_files(&self) -> Vec<PathBuf> {
        [&self.app, &self.settings_app]
            .into_iter()
            .flat_map(|bundle| {
                [
                    bundle.join("Contents/Info.plist"),
                    bundle.join("Contents/MacOS/Suchi"),
                    bundle.join("Contents/Resources/Suchi.icns"),
                ]
            })
            .collect()
    }
}
