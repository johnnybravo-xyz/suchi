# Suchi on Linux

Extract the complete native Linux package and run `./Install.sh` from your
desktop session. **Check your computer** shows which required tools are ready,
missing, or need attention, and explains what each does. You can continue only
when the checks pass. The installer then offers optional startup at login and
opens Suchi in your browser. If your file manager displays shell scripts as text,
open a terminal in the extracted folder and run `./Install.sh` there.

Suchi installs only for your user. Do not run the installer with `sudo`.
This installer requires a graphical Linux session with a systemd user manager;
it is not a system-wide service or a server/container installer. Native
packages are separate from the beta.2 binaries, which lack this installer.

## Prerequisites

Install Zenity first to use the graphical installer. Its **Installation guide**
shows the commands below; **Check again** refreshes the tool status after you
install them. Missing English OCR data and an installed ImageMagick that cannot
write PDFs are shown separately from missing executables. Pick the command for
your distribution; the installer never runs these commands or changes system
policy itself.

```sh
# Debian / Ubuntu
sudo apt install zenity xdg-utils qpdf poppler-utils tesseract-ocr tesseract-ocr-eng imagemagick

# Fedora
sudo dnf install zenity xdg-utils qpdf poppler-utils tesseract tesseract-langpack-eng ImageMagick

# Arch
sudo pacman -S zenity xdg-utils qpdf poppler tesseract tesseract-data-eng imagemagick
```

These provide PDF/image processing and English OCR, not every optional format.
ImageMagick must permit writing PDFs. If the installer's capability probe
fails, review your distribution's ImageMagick policy without broadly enabling
PDF reading or unrelated delegates. Additional formats need the tools listed
in the [supported formats guide](https://docs.suchi.page/formats).

## Open, stop, update, remove

- Open **Suchi** from your applications menu. First launch displays a one-time
  setup token: copy it, choose **Open Suchi**, and create your archive account.
  The browser opens only after Suchi is ready; another app occupying its port
  is an error, not a successful startup.
  Its address is `http://suchi.localhost:8000` (or your chosen port). Current
  Firefox and Chromium/Chrome resolve this directly to your own computer;
  no hosts-file or DNS setup is needed. Updating from the numeric address may
  require signing in again because browsers keep the two addresses separate.
- Right-click the application shortcut for **Settings** or **Stop Suchi**.
  Settings can open the archive, enable/disable login startup, stop processing,
  or uninstall. Disabling login startup does not stop the current process.
  If your desktop does not expose shortcut actions, run
  `~/.local/share/suchi-local/program/launch.sh --settings`.
- To update, extract the next package and run its `Install.sh`. The existing
  archive and configuration are preserved. The login checkbox reflects the
  current preference; changing it changes that preference.
- Uninstall asks for confirmation and removes only the program/service/shortcut.
  Your documents, keys and configuration stay on disk. Reinstallation opens
  the same archive.

The default archive is `~/.local/share/suchi-local/archive`; configuration is
`~/.config/suchi-local/local-install.toml`. `XDG_DATA_HOME` and
`XDG_CONFIG_HOME` are honored. Back up the complete archive directory, not
only its SQLite database. Closing the browser does not stop Suchi. When the
computer sleeps, email intake and background processing pause.

For first-run setup, storage, backups, optional tools and local-only limitations,
see [On your computer](https://docs.suchi.page/local-install). Shared links and
phone access are not remotely reachable from this loopback-only installation.

## Ownership

- `Install.sh` is the native-dialog entry point for an extracted package;
  from a checkout it uses `dist/suchi` after `make build`.
- `install.sh BINARY [--port N] [--start-at-login | --no-start-at-login]` installs the per-user
  program, config, systemd unit, and Desktop Entry. `--uninstall` stops and
  removes only the managed application files, preserving archive and config.
- `launch.sh [--settings | --stop]` owns local controls, waits for systemd
  readiness, presents a pending bootstrap token
  through Zenity stdin, then opens the browser. Cancelling setup leaves the
  browser closed.
- `test.sh [BINARY]` exercises install/update/uninstall and launcher behavior
  in temporary directories with mocked commands. An optional built binary
  verifies actual CLI compatibility without starting the server.

The service uses a clean environment and fixed loopback/data paths. Updates
keep configuration and the previous port unless supplied. The CLI preserves
login startup unless either login flag is supplied. A running service is stopped before replacing its executable and
restarted afterward. The installer refuses unrelated same-name services,
drop-ins, and symlinked target directories rather than adopting them.
No package downloads, updater, hosts-file changes, or system-wide service.

## Verification

From the repository root:

```sh
make local-install-check
shellcheck deploy/local-linux/*.sh hack/smoke-local-install.sh
make build
bash deploy/local-linux/test.sh ./dist/suchi
make smoke-local-install
# Optional: actual native dialogs in an isolated X display.
xvfb-run -a bash deploy/local-linux/test-dialogs.sh
```

`make check` and CI include the mocked lifecycle check, including GUI cancel,
login controls, stop, confirmed uninstall, and extracted-package installation.
When available,
`systemd-analyze` also validates a generated unit with spaces in its paths.

The optional dialog check needs Zenity, Xvfb and xdotool. It renders the actual
dialogs with test dependency states and exercises the guide, retry, continue
and cancellation paths, including window-width limits. It never installs
Suchi or modifies a real service. The native Linux workflow runs this check.

The native smoke requires a working Linux user manager, parsing tools,
`curl`, `jq`, and ImageMagick's DejaVu-Sans font. It runs a uniquely named
transient user service with a temporary archive and verifies private bootstrap,
authentication, receipt OCR/search, and restart persistence. It does not install
a desktop entry or exercise Zenity. Cleanup affects only that test instance.

Before calling a desktop distribution supported, use a disposable user profile
to verify the actual dialog, app-menu launch, login/reboot behavior, update,
and uninstall with a complete archive and keys. macOS/Windows need their own
native verification; cross-compilation is not sufficient.
