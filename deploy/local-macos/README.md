# Suchi on macOS

This unsigned local-install preview runs Suchi as your desktop user, opens
your normal browser, and requires neither Docker nor a hosted server.
It is not a signed/notarized consumer download yet. Do not disable Gatekeeper
or remove quarantine attributes to make an untrusted download run.

## Install

Install the document tools with [Homebrew](https://brew.sh), if you use it:

```sh
brew install qpdf poppler tesseract imagemagick
```

Suchi checks for English OCR data and actually converts an image to PDF;
an executable's presence alone is insufficient. The installer does not install
Homebrew, download packages, or change ImageMagick's security policy. Helpers
are found in `/opt/homebrew/bin` and `/usr/local/bin`, covering Apple Silicon
and Intel Homebrew installations. Other formats need their own
[format dependencies](https://docs.suchi.page/formats), including AnyDoc for
Office and EPUB extraction.

In an extracted, verified local-install bundle, open Terminal in that folder
and run `bash ./install.sh` to open guided setup. Supplying a binary path or
flags uses command-line installation instead.
The **Check your computer** step always shows the installed and missing
requirements. PDF tools, scanned-text recognition, image-to-PDF conversion,
and native macOS integration each report **Ready**, **Missing**, or **Needs
attention**. A tool that is installed but cannot run is not treated as ready.
Use **Install guide** for commands and guidance, then **Check again**;
**Continue** appears only when all required checks pass. Suchi never runs
package-manager commands for you.

The installer then asks whether Suchi should start at login and offers to
open the app. Updates preselect your existing login preference. It installs **Suchi.app** and **Suchi
Settings.app** in your own `Applications` folder. On the first launch, copy the
one-time token from the native dialog into the browser setup form to create
your account. No token enters a URL or the clipboard automatically.

From a source checkout, build Suchi and run:

```sh
make build
./deploy/local-macos/install.sh ./dist/suchi
open "$HOME/Applications/Suchi.app"
```

Run without `sudo`, from your graphical desktop login session. If port 8000
is occupied, rerun the installer with `--port 8001` or another unused port.
It binds only `127.0.0.1`; no hostname or hosts-file changes are required.
Paths containing spaces work. Quoting/escaping characters, non-ASCII home
paths, symlinked install directories, and unrelated same-name apps/services
are rejected rather than silently adopted.

## Everyday controls

Open **Suchi** for your archive. Closing its browser tab does not stop
processing. **Suchi Settings** opens native controls to change start-at-login,
stop processing, locate the archive, or uninstall. These local lifecycle
controls are separate from archive preferences in the browser.

Without login startup, the launchd job is registered only when you open Suchi.
Once started, launchd owns its lifetime and restarts it after exit. Opting into
login startup installs a copy of the same job in `~/Library/LaunchAgents`.
Stopping Suchi unloads that running job; opening the app loads it again.
An enabled login preference takes effect at your next login too.

The launcher verifies the registered service definition by file identity, so
macOS path casing does not falsely identify Suchi as an unrelated job.
The browser opens only after the job registered from this installation owns
the exact loopback listener and responds to readiness checks. An unrelated
server listening on the same port is not mistaken for Suchi.

## Your files, updates, and uninstall

Everything below is under `~/Library/Application Support/Suchi/`:

| Directory | Contents |
| --- | --- |
| `archive/` | Database, original documents, derived files, encryption keys |
| `config/` | `local-install.toml`, optional boot configuration |
| `program/` | Installed executable, scripts, icon, and service definition |
| `runtime/` | Pending private setup token, `server.log`, and the previous manually started session's log |

These directories are private to your OS user. The service receives a clean
environment and does not inherit a developer archive, API key, or demo mode
from your shell.

Before updating, stop Suchi and back up the complete `archive/` and `config/`
directories, not just SQLite. Rerun the installer with the verified new binary.
An update preserves configuration, the saved port unless explicitly supplied,
and login preference. A running job is stopped before replacing its binary
and restarted after installation. Gatekeeper/quarantine metadata is preserved
when copying the executable.

Uninstall from **Suchi Settings**, or run the retained installer:

```sh
"$HOME/Library/Application Support/Suchi/program/install.sh" --uninstall
```

Uninstall removes the owned apps, program files, and login job. It keeps
the archive, keys, configuration, logs, and any unrelated extra files.
Reinstalling reuses the retained archive. See
[backup and restore](https://docs.suchi.page/backup-restore) before storing
important documents; a live archive should not sit in iCloud Drive or another
file-sync folder.

## Troubleshooting and limits

- Open `runtime/server.log` for startup or parser errors. A port conflict is
  reported without opening your browser against the conflicting server.
  Opening a stopped service keeps its old log as `server.previous.log`.
  Logs are not continuously rotated while the service runs.
- macOS **System Settings → General → Login Items** can block background jobs.
  Check that setting if `launchctl` refuses to load or start Suchi.
- The computer must be awake for processing or mail intake. A loopback archive
  cannot provide share links or phone access to another device.
- Native parsers run as your user; Suchi's subprocess limits are not an OS
  security sandbox. Optional mail/model integrations still require explicit
  configuration and consent.
- This preview does not include signing credentials, notarization, automatic
  updates, or a bundled document-tool runtime.

## Verification for maintainers

```sh
bash deploy/local-macos/test.sh
shellcheck deploy/local-macos/*.sh
make build
bash deploy/local-macos/test-service.sh ./dist/suchi
```

The lifecycle test uses temporary directories and mocked `launchctl`, browser,
and dialog commands. It also executes the launcher entrypoints with the system
Bash to verify cold-start, already-stopped, and unexpected-error handling.
On macOS it additionally runs native `plutil`, `sips`,
`iconutil`, and compiles every AppleScript dialog. It does not install a real
service or change your desktop settings.

`test-service.sh` separately starts a uniquely named launchd job with a
temporary archive, validates the real job PID/path and exact loopback listener,
private setup-token delivery, and graceful stop/restart. It requires an active
macOS desktop login session but shows no dialogs. Cleanup removes only that
test job and its temporary files, never an installed Suchi service.

Native desktop acceptance still needs guided installation, first-run dialog,
repeated launch, sleep/wake, login/reboot, update, stop, complete restore, and
data-preserving uninstall on a real Mac. Cross-compilation and mocked tests
alone do not establish those behaviors.
