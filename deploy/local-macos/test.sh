#!/bin/bash
# The tests deliberately source functions with subshell-local path variables.
# shellcheck disable=SC2031,SC2329
set -euo pipefail

scripts=$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)
bash "$scripts/test-checks.sh"
bash "$scripts/test-entrypoints.sh"
test_dir=$(mktemp -d)
# These commands touch only this test-owned directory, never a real login job.
trap 'rm -rf "$test_dir"' EXIT
test_user_home=$test_dir/user\ with\ spaces
mkdir -p "$test_user_home"
commands=$test_dir/commands
browser=$test_dir/browser
dialog=$test_dir/dialog
loaded_job=$test_dir/loaded-job
native_os=$(uname -s)
listener_mode=owned
dialog_cancel=0
health_fails=0
bootstrap_fails=0

# shellcheck source=deploy/local-macos/install.sh
source "$scripts/install.sh"
desktop_home() { printf '%s\n' "$test_user_home"; }
uname() { printf 'Darwin\n'; }
id() { printf '501\n'; }
check_dependencies() { :; }
sleep() { :; }
kill() { return 1; }
stat() {
    if [[ $native_os == Darwin ]]; then command stat "$@"; else command stat -c %a "$3"; fi
}
launchctl() {
    printf '%s\n' "$*" >> "$commands"
    case "$1" in
        print)
            [[ $2 != gui/501 ]] || return 0
            [[ -f $loaded_job ]] || return 1
            printf 'gui/501/page.suchi.local = {\n\tpath = %s\n\tpid = 12345\n}\n' "$(< "$loaded_job")" ;;
        bootstrap)
            [[ $bootstrap_fails == 0 ]] || return 1
            printf '%s\n' "$3" > "$loaded_job" ;;
        bootout) rm -f "$loaded_job" ;;
        kickstart) [[ -f $loaded_job ]] ;;
        *) printf 'Unexpected launchctl command\n' >&2; return 1 ;;
    esac
}
plutil() {
    if [[ $native_os == Darwin ]]; then /usr/bin/plutil "$@"; elif command -v xmllint >/dev/null; then xmllint --noout "$2"; fi
}
sips() {
    if [[ $native_os == Darwin ]]; then /usr/bin/sips "$@"; else cp "$4" "$6"; fi
}
iconutil() {
    if [[ $native_os == Darwin ]]; then /usr/bin/iconutil "$@"; else printf 'test icon\n' > "$4"; fi
}
ditto() {
    if [[ $native_os == Darwin ]]; then /usr/bin/ditto "$@"; else cp "$4" "$5"; fi
}
osascript() {
    printf '%s\n' "$*" >> "$dialog"
    cat >> "$dialog"
    if [[ $dialog_cancel == 1 ]]; then printf 'Cancel\n'; fi
}
open() { printf '%s\n' "$*" >> "$browser"; }
lsof() {
    printf '%s\n' "lsof $*" >> "$commands"
    case "$listener_mode" in
        owned) printf 'p12345\nf9\nn127.0.0.1:18765\n' ;;
        foreign) printf 'p99999\nf9\nn127.0.0.1:18765\n' ;;
        wildcard) printf 'p12345\nf9\nn*:18765\n' ;;
        *) return 1 ;;
    esac
}
curl() { [[ $health_fails == 0 ]]; }

if [[ $# -gt 0 ]]; then
    install -m 700 "$1" "$test_dir/suchi"
else
    printf '#!/bin/sh\nprintf "serve --setup-token-file\\n"\n' > "$test_dir/suchi"
    chmod +x "$test_dir/suchi"
fi
if [[ $native_os == Darwin ]]; then /usr/bin/xattr -w page.suchi.local-test preserved "$test_dir/suchi"; fi
printf '/Library/LaunchAgents/foreign.plist\n' > "$loaded_job"
if (main "$test_dir/suchi") > /dev/null 2>&1; then printf 'Adopted an unrelated loaded job\n' >&2; exit 1; fi
[[ ! -d $test_user_home/Library/Application\ Support/Suchi ]]
rm "$loaded_job"
main "$test_dir/suchi" --port 18765 > "$test_dir/install-output"
program=$test_user_home/Library/Application\ Support/Suchi/program
archive=$test_user_home/Library/Application\ Support/Suchi/archive
runtime=$test_user_home/Library/Application\ Support/Suchi/runtime
config=$test_user_home/Library/Application\ Support/Suchi/config/local-install.toml
app=$test_user_home/Applications/Suchi.app
settings_app=$test_user_home/Applications/Suchi\ Settings.app
login_plist=$test_user_home/Library/LaunchAgents/page.suchi.local.plist
[[ -x $program/suchi && -x $app/Contents/MacOS/Suchi && -x $settings_app/Contents/MacOS/Suchi ]]
if [[ $native_os == Darwin ]]; then [[ $(/usr/bin/xattr -p page.suchi.local-test "$program/suchi") == preserved ]]; fi
[[ $(stat -f '%Lp' "$archive") == 700 && $(stat -f '%Lp' "$config") == 600 ]]
[[ ! -e $login_plist && ! -e $loaded_job ]]
grep -q 'LISTEN_ADDR=127.0.0.1:18765' "$program/page.suchi.local.plist"
grep -q '<string>/usr/bin/env</string><string>-i</string>' "$program/page.suchi.local.plist"
grep -q '<key>KeepAlive</key><true/>' "$program/page.suchi.local.plist"
if grep -q 'bootstrap\|kickstart' "$commands"; then printf 'Installer started Suchi before a launch\n' >&2; exit 1; fi

# Accept another path to the same plist, but never a different file with its contents.
ln "$program/page.suchi.local.plist" "$test_dir/alias.plist"
state=$(printf 'path = %s\npid = 12345\n' "$test_dir/alias.plist")
[[ $(job_pid "$state" "$program/page.suchi.local.plist") == 12345 ]]
rm "$test_dir/alias.plist"
cp "$program/page.suchi.local.plist" "$test_dir/alias.plist"
if job_pid "$state" "$program/page.suchi.local.plist" >/dev/null 2>&1; then
    printf 'Accepted a different service definition with identical contents\n' >&2; exit 1
fi

printf 'keep documents and keys\n' > "$archive/keep"
printf 'admin_email = "private@example.test"\n' > "$config"
printf '%s\n' "$program/page.suchi.local.plist" > "$loaded_job"
install() { return 1; }
if (main "$test_dir/suchi") > /dev/null 2>&1; then printf 'Ignored staging failure\n' >&2; exit 1; fi
unset -f install
[[ -f $loaded_job && -x $program/suchi ]]
if compgen -G "$program/.stage.*" >/dev/null; then printf 'Leaked staged files\n' >&2; exit 1; fi
iconutil() { return 1; }
if (main "$test_dir/suchi") >/dev/null 2>&1; then printf 'Ignored failed icon preparation\n' >&2; exit 1; fi
[[ -f $loaded_job && -x $program/suchi ]]
iconutil() {
    if [[ $native_os == Darwin ]]; then /usr/bin/iconutil "$@"; else printf 'test icon\n' > "$4"; fi
}
main "$test_dir/suchi" --start-at-login > "$test_dir/update-output"
if [[ $native_os == Darwin ]]; then [[ $(/usr/bin/xattr -p page.suchi.local-test "$program/suchi") == preserved ]]; fi
[[ $(< "$program/port") == 18765 && $(< "$program/start-at-login") == 1 ]]
[[ $(< "$loaded_job") == "$login_plist" && -f $login_plist ]]
grep -q admin_email "$config"
grep -q 'bootout gui/501/page.suchi.local' "$commands"
main "$test_dir/suchi" > /dev/null
[[ $(< "$program/start-at-login") == 1 ]]
main "$test_dir/suchi" --no-start-at-login > /dev/null
[[ $(< "$program/start-at-login") == 0 && ! -e $login_plist ]]
check_dependencies() { printf 'Missing parser\n' >&2; return 1; }
set_login_start "$program" 1
[[ $(< "$program/start-at-login") == 1 && $(< "$loaded_job") == "$login_plist" ]]
set_login_start "$program" 0
[[ $(< "$program/start-at-login") == 0 && ! -e $login_plist && $(< "$loaded_job") == "$program/page.suchi.local.plist" ]]
check_dependencies() { :; }

# shellcheck source=deploy/local-macos/launch.sh
source "$scripts/launch.sh"
desktop_home() { printf '%s\n' "$test_user_home"; }
script_dir=$program
printf 'private-test-token\n' > "$runtime/setup-token"
chmod 600 "$runtime/setup-token"
main
[[ $(< "$browser") == http://127.0.0.1:18765 ]]
grep -q -- "$runtime/setup-token" "$dialog"
if grep -q 'private-test-token' "$dialog"; then printf 'Token appeared in AppleScript source or argv\n' >&2; exit 1; fi
rm "$browser"
for listener_mode in foreign wildcard missing; do
    if main >/dev/null 2>&1; then printf 'Opened a browser without exact Suchi listener ownership\n' >&2; exit 1; fi
    [[ ! -e $browser ]]
done
listener_mode=owned
health_fails=1
if main >/dev/null 2>&1; then printf 'Ignored failed readiness\n' >&2; exit 1; fi
[[ ! -e $browser ]]
health_fails=0
dialog_cancel=1
main >/dev/null 2>&1
[[ ! -e $browser ]]
dialog_cancel=0
printf '/Library/LaunchAgents/foreign.plist\n' > "$loaded_job"
if main >/dev/null 2>&1; then printf 'Opened an unrelated loaded job\n' >&2; exit 1; fi
[[ ! -e $browser ]]
rm "$loaded_job"
bootstrap_fails=1
if main >/dev/null 2>&1; then printf 'Ignored bootstrap failure\n' >&2; exit 1; fi
[[ ! -e $browser ]]
bootstrap_fails=0
printf 'previous start\n' > "$runtime/server.log"
chmod 600 "$runtime/server.log"
main
[[ -f $runtime/server.previous.log && ! -e $runtime/server.log ]]
grep -q 'previous start' "$runtime/server.previous.log"

# shellcheck source=deploy/local-macos/install.sh
source "$scripts/install.sh"
desktop_home() { printf '%s\n' "$test_user_home"; }
if (main "$test_dir/suchi" --port 80) >/dev/null 2>&1; then printf 'Accepted a privileged port\n' >&2; exit 1; fi
printf 'Unrelated app\n' > "$app/Contents/Info.plist"
if (main --uninstall) >/dev/null 2>&1; then printf 'Removed an unrelated app\n' >&2; exit 1; fi
[[ -f $program/suchi && -f $archive/keep ]]
printf '<!-- Managed by Suchi local installer. -->\n' > "$app/Contents/Info.plist"
printf 'user file\n' > "$program/keep"
main --uninstall > "$test_dir/uninstall-output"
[[ ! -e $app && ! -e $settings_app && ! -e $login_plist && ! -e $program/suchi ]]
[[ -f $archive/keep && -f $config && -f $program/keep && ! -e $runtime/setup-token ]]
grep -q 'were kept' "$test_dir/uninstall-output"

# With no bundled binary, the actual no-argument entrypoint opens its chooser.
mkdir "$test_dir/entrypoint"
cp "$scripts/install.sh" "$scripts/common.sh" "$test_dir/entrypoint/"
osascript() {
    local script
    script=$(cat)
    printf '%s\n' "$script" >> "$dialog"
    return 1
}
export dialog browser
export -f osascript open
rm -f "$browser" "$dialog"
/bin/bash "$test_dir/entrypoint/install.sh" --help > "$test_dir/help-output"
grep -q 'Run without arguments for guided setup' "$test_dir/help-output"
[[ ! -e $dialog && ! -e $browser ]]
/bin/bash "$test_dir/entrypoint/install.sh" > "$test_dir/gui-output" 2>&1
grep -q 'choose file' "$dialog"
[[ ! -e $browser && ! -e $loaded_job ]]

if [[ $native_os == Darwin ]]; then
    # Compile each actual UI script without showing a dialog in CI.
    for source_file in launch.sh settings.sh install.sh; do
        awk -v prefix="$test_dir/$source_file-" '/<<.APPLESCRIPT./ {n++; capture=1; next} /^APPLESCRIPT$/ {capture=0; next} capture {print > (prefix n ".applescript")}' "$scripts/$source_file"
    done
    for source_file in "$test_dir"/*.applescript; do /usr/bin/osacompile -o "$source_file.scpt" "$source_file" >/dev/null; done
fi
printf 'Local macOS installer and launcher tests passed.\n'
