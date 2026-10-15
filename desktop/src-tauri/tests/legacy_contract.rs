// SPDX-License-Identifier: AGPL-3.0-or-later

#[path = "support/legacy_fixtures.rs"]
mod legacy_fixtures;

use std::path::{Path, PathBuf};

use legacy_fixtures::{LegacyLinuxFixture, LegacyMacFixture, MANAGED_FILE_MARKER, MANAGED_MARKER, MANAGED_XML_MARKER};

#[test]
fn final_linux_installer_contract_is_exact() {
    let home = Path::new("/home/alice");
    let fixture = LegacyLinuxFixture::build(
        home,
        Path::new("/home/alice/.local/share"),
        Path::new("/home/alice/.config"),
        18_765,
        true,
    );

    assert_eq!(fixture.archive, PathBuf::from("/home/alice/.local/share/suchi-local/archive"));
    assert_eq!(fixture.config, PathBuf::from("/home/alice/.config/suchi-local/local-install.toml"));
    assert_eq!(fixture.program, PathBuf::from("/home/alice/.local/share/suchi-local/program"));
    assert_eq!(fixture.marker, fixture.program.join("managed-by-suchi-local"));
    assert_eq!(fixture.port_file, fixture.program.join("port"));
    assert_eq!(fixture.unit, PathBuf::from("/home/alice/.config/systemd/user/suchi-local.service"));
    assert_eq!(fixture.desktop_entry, PathBuf::from("/home/alice/.local/share/applications/suchi-local.desktop"));
    assert_eq!(fixture.port, 18_765);
    assert!(fixture.login_enabled);
    assert_eq!(MANAGED_MARKER, "Managed by Suchi local installer.\n");
    assert!(fixture.unit_contents.starts_with(&format!("{MANAGED_FILE_MARKER}\n[Unit]")));
    for field in [
        "Type=notify",
        "NotifyAccess=main",
        "RuntimeDirectory=suchi-local",
        "RuntimeDirectoryMode=0700",
        "LISTEN_ADDR=127.0.0.1:18765",
        "PUBLIC_URL=http://suchi.localhost:18765",
        "--setup-token-file=%t/suchi-local/setup-token",
        "WantedBy=default.target",
    ] {
        assert!(fixture.unit_contents.contains(field), "missing unit field: {field}");
    }
    for field in [
        "Name=Suchi",
        "Categories=Office;",
        "Actions=Settings;Stop;",
        "--settings",
        "--stop",
    ] {
        assert!(fixture.desktop_contents.contains(field), "missing desktop field: {field}");
    }
    assert_eq!(fixture.owned_program_files.len(), 7);
}

#[test]
fn final_macos_installer_contract_is_exact() {
    let home = Path::new("/Users/alice");
    let fixture = LegacyMacFixture::build(home, 18_765, true);

    assert_eq!(fixture.root, PathBuf::from("/Users/alice/Library/Application Support/Suchi"));
    assert_eq!(fixture.archive, fixture.root.join("archive"));
    assert_eq!(fixture.config, fixture.root.join("config/local-install.toml"));
    assert_eq!(fixture.runtime, fixture.root.join("runtime"));
    assert_eq!(fixture.program, fixture.root.join("program"));
    assert_eq!(fixture.marker, fixture.program.join("managed-by-suchi-local"));
    assert_eq!(fixture.service_plist, fixture.program.join("page.suchi.local.plist"));
    assert_eq!(fixture.login_plist, PathBuf::from("/Users/alice/Library/LaunchAgents/page.suchi.local.plist"));
    assert_eq!(fixture.app, PathBuf::from("/Users/alice/Applications/Suchi.app"));
    assert_eq!(fixture.settings_app, PathBuf::from("/Users/alice/Applications/Suchi Settings.app"));
    assert_eq!(fixture.port_file, fixture.program.join("port"));
    assert_eq!(fixture.login_preference_file, fixture.program.join("start-at-login"));
    assert_eq!(fixture.port, 18_765);
    assert!(fixture.login_enabled);
    assert_eq!(MANAGED_XML_MARKER, "<!-- Managed by Suchi local installer. -->");
    for field in [
        "<key>Label</key><string>page.suchi.local</string>",
        "<string>LISTEN_ADDR=127.0.0.1:18765</string>",
        "<string>PUBLIC_URL=http://127.0.0.1:18765</string>",
        "<string>--setup-token-file=/Users/alice/Library/Application Support/Suchi/runtime/setup-token</string>",
        "<key>KeepAlive</key><true/>",
        "<key>ExitTimeOut</key><integer>60</integer>",
        "<key>Umask</key><integer>63</integer>",
    ] {
        assert!(fixture.service_contents.contains(field), "missing plist field: {field}");
    }
    assert_eq!(fixture.owned_program_files.len(), 11);
    assert_eq!(fixture.owned_app_files().len(), 6);
}
