#!/usr/bin/env bash
set -euo pipefail

scripts=$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)
test_dir=$(mktemp -d)
# Only this test-owned directory is removed; no services or desktop files are touched.
trap 'rm -rf -- "$test_dir"' EXIT
export XDG_DATA_HOME="$test_dir/data with spaces" XDG_CONFIG_HOME="$test_dir/config" XDG_RUNTIME_DIR="$test_dir/runtime"
export commands="$test_dir/commands" browser="$test_dir/browser" dialog="$test_dir/dialog"
export start_fails=0 service_active=0 foreign_unit=0 foreign_drop_in=0 dialog_cancel=0
mkdir -p "$XDG_RUNTIME_DIR"

systemctl() {
    printf '%s\n' "$*" >> "$commands"
    case "$*" in
        *'show --property=LoadState'*)
            if [[ $foreign_drop_in == 1 ]]; then
                printf 'LoadState=loaded\nFragmentPath=%s/systemd/user/suchi-local.service\nDropInPaths=/etc/systemd/user/service.d/custom.conf\n' "$XDG_CONFIG_HOME"
            elif [[ $foreign_unit == 1 ]]; then
                printf 'LoadState=loaded\nFragmentPath=/etc/systemd/user/suchi-local.service\nDropInPaths=\n'
            elif [[ -f $XDG_CONFIG_HOME/systemd/user/suchi-local.service ]]; then
                printf 'LoadState=loaded\nFragmentPath=%s/systemd/user/suchi-local.service\nDropInPaths=\n' "$XDG_CONFIG_HOME"
            else
                printf 'LoadState=not-found\nFragmentPath=\nDropInPaths=\n'
            fi ;;
        *is-active*) [[ $service_active == 1 ]] ;;
        *'start suchi-local.service') [[ $start_fails == 0 ]] ;;
    esac
}
zenity() {
    printf '%s\n' "$*" >> "$dialog"
    if [[ $1 == --text-info ]]; then cat >> "$dialog"; [[ $dialog_cancel == 0 ]]; fi
}
xdg-open() { printf '%s\n' "$*" >> "$browser"; }
export -f systemctl zenity xdg-open

# Package and graphics capability probes need real helpers; these tests exercise
# installation ownership, generated configuration, updates and launcher behavior.
# shellcheck source=deploy/local-linux/install.sh
source "$scripts/install.sh"
check_dependencies() { :; }
if [[ $# -gt 0 ]]; then
    install -m 700 -- "$1" "$test_dir/suchi"
else
    printf '#!/bin/sh\nprintf "serve --systemd --setup-token-file\\n" >&2\n' > "$test_dir/suchi"
    chmod +x "$test_dir/suchi"
fi
foreign_unit=1
if (main "$test_dir/suchi") > /dev/null 2>&1; then printf 'Shadowed a foreign user service\n' >&2; exit 1; fi
[[ ! -d $XDG_DATA_HOME/suchi-local ]]
foreign_unit=0
foreign_drop_in=1
if (main "$test_dir/suchi") > /dev/null 2>&1; then printf 'Accepted an unexpected service drop-in\n' >&2; exit 1; fi
[[ ! -d $XDG_DATA_HOME/suchi-local ]]
foreign_drop_in=0
printf '#!/bin/sh\nprintf "old suchi help\\n"\n' > "$test_dir/old-suchi"
chmod +x "$test_dir/old-suchi"
if (main "$test_dir/old-suchi") > /dev/null 2>&1; then printf 'Accepted an incompatible Suchi binary\n' >&2; exit 1; fi
[[ ! -d $XDG_DATA_HOME/suchi-local ]]
main "$test_dir/suchi" --port 18765 > "$test_dir/install-output"
program=$XDG_DATA_HOME/suchi-local/program
archive=$XDG_DATA_HOME/suchi-local/archive
config=$XDG_CONFIG_HOME/suchi-local/local-install.toml
unit=$XDG_CONFIG_HOME/systemd/user/suchi-local.service
desktop=$XDG_DATA_HOME/applications/suchi-local.desktop
[[ -x $program/suchi && -x $program/launch.sh && -x $program/install.sh ]]
[[ $(stat -c %a "$archive") == 700 && $(stat -c %a "$config") == 600 ]]
grep -q 'Type=notify' "$unit"
grep -q 'LISTEN_ADDR=127.0.0.1:18765' "$unit"
grep -q "NOTIFY_SOCKET=\${NOTIFY_SOCKET}" "$unit"
grep -q 'ExecStart=/usr/bin/env -i' "$unit"
if command -v systemd-analyze > /dev/null; then
    systemd-analyze --user verify "$unit"
fi
if grep -q 'enable\| start ' "$commands"; then printf 'Installation started or enabled the service without opt-in\n' >&2; exit 1; fi

printf 'archive sentinel\n' > "$archive/keep"
printf 'admin_email = "local@example.test"\n' > "$config"
service_active=1
install() { return 1; }
if (main "$test_dir/suchi") > /dev/null 2>&1; then printf 'Ignored failed binary staging\n' >&2; exit 1; fi
unset -f install
if grep -q -- '--user stop' "$commands"; then printf 'Stopped the service before staging succeeded\n' >&2; exit 1; fi
if compgen -G "$program/.suchi.*" > /dev/null; then printf 'Left staged binaries after copy failure\n' >&2; exit 1; fi
[[ -x $program/suchi ]]
main "$test_dir/suchi" --start-at-login > "$test_dir/update-output"
[[ $(< "$program/port") == 18765 ]]
grep -q 'admin_email' "$config"
grep -q -- '--user stop suchi-local.service' "$commands"
grep -q -- '--user enable suchi-local.service' "$commands"
grep -q -- '--user start suchi-local.service' "$commands"

mkdir -p "$XDG_RUNTIME_DIR/suchi-local"
printf 'one-time-test-token\n' > "$XDG_RUNTIME_DIR/suchi-local/setup-token"
bash "$program/launch.sh"
[[ $(< "$browser") == http://127.0.0.1:18765 ]]
grep -qx 'one-time-test-token' "$dialog"
if grep '^--' "$dialog" | grep -q 'one-time-test-token'; then printf 'Token leaked into dialog arguments\n' >&2; exit 1; fi

rm "$browser" "$dialog"
start_fails=1
if bash "$program/launch.sh"; then printf 'Launcher opened after startup failure\n' >&2; exit 1; fi
[[ ! -e $browser ]]
grep -q -- '--error' "$dialog"
start_fails=0
dialog_cancel=1
if bash "$program/launch.sh"; then printf 'Launcher ignored setup-dialog cancellation\n' >&2; exit 1; fi
[[ ! -e $browser ]]
dialog_cancel=0

if (main "$test_dir/suchi" --port 80) > /dev/null 2>&1; then printf 'Accepted a privileged port\n' >&2; exit 1; fi
if (check_path '/tmp/unsafe%h') > /dev/null 2>&1; then printf 'Accepted a systemd specifier in a path\n' >&2; exit 1; fi

printf '[Unit]\nDescription=Unrelated user service\n' > "$unit"
if (main --uninstall) > /dev/null 2>&1; then printf 'Removed an unrelated unit\n' >&2; exit 1; fi
[[ -f $program/suchi && -f $archive/keep ]]
printf '# Managed by Suchi local installer.\n' > "$unit"
main --uninstall > "$test_dir/uninstall-output"
[[ ! -e $program && ! -e $unit && ! -e $desktop ]]
[[ -f $archive/keep && -f $config ]]
grep -q 'were kept' "$test_dir/uninstall-output"
printf 'Local Linux installer and launcher tests passed.\n'
