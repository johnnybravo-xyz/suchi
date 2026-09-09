# Local Linux installer

See [On your computer](../../docs/local-install.mdx) for installation,
first-run setup, updates, storage paths, uninstall, and limitations.
This source-checkout preview is not included in the beta.2 binary.

## Ownership

- `install.sh BINARY [--port N] [--start-at-login]` installs the per-user
  program, config, systemd unit, and Desktop Entry. `--uninstall` stops and
  removes only the managed application files, preserving archive and config.
- `launch.sh` waits for systemd readiness, presents a pending bootstrap token
  through Zenity stdin, then opens the browser. Cancelling setup leaves the
  browser closed.
- `test.sh [BINARY]` exercises install/update/uninstall and launcher behavior
  in temporary directories with mocked commands. An optional built binary
  verifies actual CLI compatibility without starting the server.

The service uses a clean environment and fixed loopback/data paths. Updates
keep configuration, the previous port unless supplied, and login-startup
preference. A running service is stopped before replacing its executable and
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
```

`make check` and CI include the mocked lifecycle check. When available,
`systemd-analyze` also validates a generated unit with spaces in its paths.

The native smoke requires a working Linux user manager, parsing tools,
`curl`, `jq`, and ImageMagick's DejaVu-Sans font. It runs a uniquely named
transient user service with a temporary archive and verifies private bootstrap,
authentication, receipt OCR/search, and restart persistence. It does not install
a desktop entry or exercise Zenity. Cleanup affects only that test instance.

Before calling a desktop distribution supported, use a disposable user profile
to verify the actual dialog, app-menu launch, login/reboot behavior, update,
and uninstall with a complete archive and keys. macOS/Windows need their own
native verification; cross-compilation is not sufficient.
