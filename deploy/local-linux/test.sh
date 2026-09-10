#!/usr/bin/env bash
set -euo pipefail

scripts=$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)
test_dir=$(mktemp -d)
# Only this test-owned directory is removed; no services or desktop files are touched.
trap 'rm -rf -- "$test_dir"' EXIT
export XDG_DATA_HOME="$test_dir/data with spaces" XDG_CONFIG_HOME="$test_dir/config" XDG_RUNTIME_DIR="$test_dir/runtime"
export commands="$test_dir/commands" browser="$test_dir/browser" dialog="$test_dir/dialog"
export browser_log="$browser" app_launch="$test_dir/app-launch"
export available_app_browser='' open_choice=default open_cancel=0 app_exit=0
export start_fails=0 reload_fails=0 service_active=0 foreign_unit=0 foreign_drop_in=0 dialog_cancel=0
export list_choice='' list_cancel=0 question_cancel=0 service_enabled=0
export dependency_cancel=0 dependency_recheck=0 dependency_fault="$test_dir/dependency-fault"
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
        *is-enabled*) [[ $service_enabled == 1 ]] ;;
        *'start suchi-local.service') [[ $start_fails == 0 ]] ;;
        *daemon-reload*)
            [[ $reload_fails == 0 ]] || { printf 'Service manager reload failed\n' >&2; return 1; } ;;
    esac
}
zenity() {
    printf '%s\n' "$*" >> "$dialog"
    case "$1" in
        --text-info) cat >> "$dialog"; [[ $dialog_cancel == 0 ]] ;;
        --list)
            [[ $list_cancel == 0 ]] || return 1
            if [[ $* == *--radiolist* ]]; then
                [[ $open_cancel == 0 ]] || return 1
                printf '%s' "$open_choice"
            elif [[ $* == *'--title=Check your computer'* ]]; then
                [[ $dependency_cancel == 0 ]] || return 1
                if [[ $dependency_recheck == 1 && -f $dependency_fault ]]; then rm -- "$dependency_fault"; fi
            else
                printf '%s' "$list_choice"
            fi ;;
        --question) [[ $question_cancel == 0 ]] ;;
    esac
}
xdg-open() { printf '%s\n' "$*" >> "$browser_log"; }
chromium() { printf '%s\n' "$@" >> "$app_launch"; return "$app_exit"; }
command() {
    if [[ ${1:-} == -v ]]; then
        case "${2:-}" in
            chromium|chromium-browser|google-chrome|google-chrome-stable)
                [[ $2 == "$available_app_browser" ]] || return 1 ;;
        esac
    fi
    builtin command "$@"
}
export -f systemctl zenity xdg-open chromium command

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
installed_program=$XDG_DATA_HOME/suchi-local/program
installed_archive=$XDG_DATA_HOME/suchi-local/archive
installed_config=$XDG_CONFIG_HOME/suchi-local/local-install.toml
unit=$XDG_CONFIG_HOME/systemd/user/suchi-local.service
desktop=$XDG_DATA_HOME/applications/suchi-local.desktop
[[ -x $installed_program/suchi && -x $installed_program/launch.sh && -x $installed_program/install.sh ]]
cmp "$scripts/../../core/ui/assets/brand/suchi-tile.svg" "$installed_program/suchi-tile.svg"
grep -qx "Icon=$installed_program/suchi-tile.svg" "$desktop"
grep -qx 'Actions=Settings;Stop;' "$desktop"
grep -q 'launch.sh" --settings' "$desktop"
grep -q 'launch.sh" --stop' "$desktop"
[[ $(stat -c %a "$installed_archive") == 700 && $(stat -c %a "$installed_config") == 600 ]]
grep -q 'Type=notify' "$unit"
grep -q 'LISTEN_ADDR=127.0.0.1:18765' "$unit"
grep -q 'PUBLIC_URL=http://suchi.localhost:18765' "$unit"
grep -q "NOTIFY_SOCKET=\${NOTIFY_SOCKET}" "$unit"
grep -q 'ExecStart=/usr/bin/env -i' "$unit"
if command -v systemd-analyze > /dev/null; then
    systemd-analyze --user verify "$unit"
fi
if grep -q 'enable\| start ' "$commands"; then printf 'Installation started or enabled the service without opt-in\n' >&2; exit 1; fi

printf 'archive sentinel\n' > "$installed_archive/keep"
printf 'admin_email = "local@example.test"\n' > "$installed_config"
printf 'chromium\n' > "$installed_program/open-with"
service_active=1
install() { return 1; }
if (main "$test_dir/suchi") > /dev/null 2>&1; then printf 'Ignored failed binary staging\n' >&2; exit 1; fi
unset -f install
if grep -q -- '--user stop' "$commands"; then printf 'Stopped the service before staging succeeded\n' >&2; exit 1; fi
if compgen -G "$installed_program/.suchi.*" > /dev/null; then printf 'Left staged binaries after copy failure\n' >&2; exit 1; fi
[[ -x $installed_program/suchi ]]
main "$test_dir/suchi" --start-at-login > "$test_dir/update-output"
[[ $(< "$installed_program/port") == 18765 ]]
[[ $(< "$installed_program/open-with") == chromium ]]
grep -q 'admin_email' "$installed_config"
grep -q -- '--user stop suchi-local.service' "$commands"
grep -q -- '--user enable suchi-local.service' "$commands"
grep -q -- '--user start suchi-local.service' "$commands"

mkdir -p "$XDG_RUNTIME_DIR/suchi-local"
printf 'default\n' > "$installed_program/open-with"
printf 'one-time-test-token\n' > "$XDG_RUNTIME_DIR/suchi-local/setup-token"
bash "$installed_program/launch.sh"
[[ $(< "$browser") == http://suchi.localhost:18765 ]]
grep -qx 'one-time-test-token' "$dialog"
if grep '^--' "$dialog" | grep -q 'one-time-test-token'; then printf 'Token leaked into dialog arguments\n' >&2; exit 1; fi

rm "$browser" "$dialog"
start_fails=1
if bash "$installed_program/launch.sh"; then printf 'Launcher opened after startup failure\n' >&2; exit 1; fi
[[ ! -e $browser ]]
grep -q -- '--error' "$dialog"
start_fails=0
dialog_cancel=1
if bash "$installed_program/launch.sh"; then printf 'Launcher ignored setup-dialog cancellation\n' >&2; exit 1; fi
[[ ! -e $browser ]]
dialog_cancel=0

# First choice, remembered app launch, and a settings-only change of preference.
rm -- "$installed_program/open-with"
available_app_browser=chromium
open_choice=chromium
bash "$installed_program/launch.sh"
for ((attempt=0; attempt<100; attempt++)); do [[ ! -s $app_launch ]] || break; sleep .02; done
[[ $(< "$app_launch") == --app=http://suchi.localhost:18765 ]]
[[ $(< "$installed_program/open-with") == chromium && ! -e $browser ]]
[[ $(stat -c %a "$installed_program/open-with") == 600 ]]
: > "$app_launch"
open_cancel=1
bash "$installed_program/launch.sh"
for ((attempt=0; attempt<100; attempt++)); do [[ ! -s $app_launch ]] || break; sleep .02; done
[[ $(< "$app_launch") == --app=http://suchi.localhost:18765 ]]
open_cancel=0
list_choice='Open with'
open_choice=default
bash "$installed_program/launch.sh" --settings
[[ $(< "$installed_program/open-with") == default && ! -e $browser ]]

# A cancelled choice neither launches a browser nor saves a preference.
rm -- "$installed_program/open-with" "$app_launch"
open_cancel=1
if bash "$installed_program/launch.sh"; then printf 'Ignored cancelled browser choice\n' >&2; exit 1; fi
[[ ! -e $installed_program/open-with && ! -e $browser && ! -e $app_launch ]]
open_cancel=0

# Removing the selected browser asks again and keeps a normal-tab option.
printf 'chromium\n' > "$installed_program/open-with"
available_app_browser=''
bash "$installed_program/launch.sh"
[[ $(< "$installed_program/open-with") == default ]]
[[ $(< "$browser") == http://suchi.localhost:18765 && ! -e $app_launch ]]
rm -- "$browser"

# Preferences are data, never executable commands; Settings can repair them.
printf 'touch %s/unsafe-browser-command\n' "$test_dir" > "$installed_program/open-with"
if bash "$installed_program/launch.sh"; then printf 'Accepted an arbitrary browser command\n' >&2; exit 1; fi
[[ ! -e $test_dir/unsafe-browser-command && ! -e $browser ]]
bash "$installed_program/launch.sh" --settings
[[ $(< "$installed_program/open-with") == default ]]
rm -- "$installed_program/open-with"
printf 'keep\n' > "$test_dir/foreign-preference"
ln -s "$test_dir/foreign-preference" "$installed_program/open-with"
if bash "$installed_program/launch.sh" --settings; then printf 'Replaced a linked preference\n' >&2; exit 1; fi
if (main --uninstall) > /dev/null 2>&1; then printf 'Uninstalled through a linked preference\n' >&2; exit 1; fi
[[ $(< "$test_dir/foreign-preference") == keep && -x $installed_program/suchi ]]
rm -- "$installed_program/open-with"
mkdir "$installed_program/open-with"
if (main --uninstall) > /dev/null 2>&1; then printf 'Uninstalled around a non-file preference\n' >&2; exit 1; fi
[[ -x $installed_program/suchi && -f $unit ]]
rmdir "$installed_program/open-with"

# An asynchronous browser failure still reports an actionable native error.
printf 'chromium\n' > "$installed_program/open-with"
available_app_browser=chromium
app_exit=1
: > "$dialog"
bash "$installed_program/launch.sh"
for ((attempt=0; attempt<100; attempt++)); do
    if grep -q -- '--error' "$dialog"; then break; fi
    sleep .02
done
grep -q -- '--error' "$dialog"
grep -q 'http://suchi.localhost:18765' "$dialog"
[[ ! -e $browser ]]
app_exit=0
available_app_browser=''
printf 'default\n' > "$installed_program/open-with"

: > "$commands"
list_cancel=1
bash "$installed_program/launch.sh" --settings
[[ $(< "$commands") == '--user is-enabled --quiet suchi-local.service' && ! -e $browser ]]
list_cancel=0
list_choice='Start at login'
bash "$installed_program/launch.sh" --settings
grep -qx -- '--user enable suchi-local.service' "$commands"
: > "$commands"
list_choice='Do not start at login'
service_enabled=1
bash "$installed_program/launch.sh" --settings
grep -qx -- '--user disable suchi-local.service' "$commands"
if grep -q -- '--now' "$commands"; then printf 'Disabling login also stopped Suchi\n' >&2; exit 1; fi
: > "$commands"
bash "$installed_program/launch.sh" --stop
grep -qx -- '--user stop suchi-local.service' "$commands"
grep -q 'resume next time you log in' "$dialog"
[[ ! -e $browser ]]
: > "$commands"
list_choice='Uninstall Suchi'
question_cancel=1
bash "$installed_program/launch.sh" --settings
[[ $(< "$commands") == '--user is-enabled --quiet suchi-local.service' && -x $installed_program/suchi && -f $installed_archive/keep ]]
grep -q -- '--default-cancel' "$dialog"
question_cancel=0

if (main "$test_dir/suchi" --port 80) > /dev/null 2>&1; then printf 'Accepted a privileged port\n' >&2; exit 1; fi
if (check_path '/tmp/unsafe%h') > /dev/null 2>&1; then printf 'Accepted a systemd specifier in a path\n' >&2; exit 1; fi

printf '[Unit]\nDescription=Unrelated user service\n' > "$unit"
if (main --uninstall) > /dev/null 2>&1; then printf 'Removed an unrelated unit\n' >&2; exit 1; fi
[[ -f $installed_program/suchi && -f $installed_archive/keep ]]
printf '# Managed by Suchi local installer.\n' > "$unit"
bash "$installed_program/launch.sh" --settings
[[ ! -e $installed_program && ! -e $unit && ! -e $desktop ]]
[[ -f $installed_archive/keep && -f $installed_config ]]
grep -q 'were kept' "$dialog"

# Exercise the same GUI and CLI files from an extracted package, without a checkout.
package=$test_dir/package
mkdir "$package"
cp "$scripts/Install.sh" "$scripts/install.sh" "$scripts/launch.sh" "$scripts/../../core/ui/assets/brand/suchi-tile.svg" "$package/"
cp "$test_dir/suchi" "$package/suchi"
(
    # shellcheck source=deploy/local-linux/Install.sh
    source "$package/Install.sh"
    # The GUI runs a separate shell, so fake the external tool boundaries rather
    # than replacing its prerequisite checks with a sourced function override.
    # shellcheck disable=SC2329
    {
        qpdf() { [[ ! -f $dependency_fault || $(< "$dependency_fault") != qpdf-broken ]]; }
        pdftotext() { :; }
        pdftoppm() { :; }
        pdfinfo() { :; }
        tesseract() {
            if [[ -f $dependency_fault && $(< "$dependency_fault") == english ]]; then printf 'deu\n'; else printf 'eng\n'; fi
        }
        magick() {
            if [[ -f $dependency_fault && $(< "$dependency_fault") == pdf ]]; then return 1; fi
            printf '%%PDF-fixture\n' > "${2#PDF:}"
        }
        command() {
            if [[ ${1:-} == -v && -f $dependency_fault && ${2:-} == "$(< "$dependency_fault")" ]]; then return 1; fi
            builtin command "$@"
        }
    }
    export -f qpdf pdftotext pdftoppm pdfinfo tesseract magick command
    : > "$commands"
    if (
        # Called indirectly by gui_main's prerequisite lookup.
        # shellcheck disable=SC2329
        command() { [[ ${2:-} != zenity ]] || return 1; builtin command "$@"; }
        gui_main
    ) > "$test_dir/missing-zenity" 2>&1; then printf 'Ignored missing Zenity\n' >&2; exit 1; fi
    grep -q 'sudo apt install' "$test_dir/missing-zenity"
    grep -q 'never installs system packages' "$test_dir/missing-zenity"
    [[ ! -s $commands && ! -d $installed_program ]]
    for fault in qpdf qpdf-broken english pdf; do
        printf '%s\n' "$fault" > "$dependency_fault"
        : > "$dialog"
        dependency_cancel=1
        gui_main
        grep -q -- '--title=Check your computer' "$dialog"
        grep -q -- '--ok-label=Check again' "$dialog"
        if grep -q -- '--ok-label=Continue' "$dialog"; then printf 'Offered Continue with failed prerequisites\n' >&2; exit 1; fi
        case "$fault" in
            qpdf) grep -q 'Missing qpdf' "$dialog" ;;
            qpdf-broken) grep -q 'Needs attention qpdf Reads and unlocks PDFs; tool could not run' "$dialog" ;;
            english) grep -q 'Missing English OCR data (eng)' "$dialog" ;;
            pdf) grep -q 'Needs attention ImageMagick PDF writing failed' "$dialog" ;;
        esac
        [[ ! -s $commands && ! -d $installed_program ]]
    done
    rm -- "$dependency_fault"
    dependency_cancel=0
    list_cancel=1
    gui_main
    [[ ! -s $commands && ! -d $installed_program ]]
    list_cancel=0
    list_choice=''
    question_cancel=1
    service_active=0
    printf 'qpdf\n' > "$dependency_fault"
    dependency_recheck=1
    : > "$dialog"
    gui_main
    dependency_recheck=0
    grep -q -- '--ok-label=Check again' "$dialog"
    grep -q -- '--ok-label=Continue' "$dialog"
    grep -q 'Ready English OCR data (eng)' "$dialog"
    grep -q 'Ready ImageMagick Creates PDFs from images; PDF writing works' "$dialog"
    [[ ! -e $dependency_fault ]]
    [[ -x $installed_program/suchi && -f $installed_archive/keep && -f $installed_config ]]
    if grep -q 'enable\| start ' "$commands"; then printf 'GUI enabled login or started without opt-in\n' >&2; exit 1; fi
    : > "$commands"
    list_choice='Start Suchi when I log in'
    question_cancel=0
    gui_main
    grep -qx -- '--user enable suchi-local.service' "$commands"
    [[ $(< "$browser") == http://suchi.localhost:8000 ]]
    service_enabled=1
    list_choice=''
    question_cancel=1
    : > "$commands"
    gui_main
    grep -q 'TRUE Start Suchi when I log in' "$dialog"
    grep -qx -- '--user disable suchi-local.service' "$commands"
    : > "$commands"
    : > "$dialog"
    rm -f -- "$browser"
    reload_fails=1
    question_cancel=0
    if gui_main > "$test_dir/gui-reload-failure" 2>&1; then
        printf 'GUI reported success after service-manager reload failure\n' >&2; exit 1
    fi
    grep -qx -- '--user daemon-reload' "$commands"
    grep -q 'Service manager reload failed' "$dialog"
    grep -q 'Suchi installation could not finish' "$dialog"
    if grep -q 'Suchi is installed' "$dialog" || [[ -e $browser ]]; then
        printf 'GUI showed success or opened the browser after failed installation\n' >&2; exit 1
    fi
    reload_fails=0
    main --uninstall > /dev/null
)
[[ ! -e $installed_program && -f $installed_archive/keep && -f $installed_config ]]
printf 'Local Linux installer and launcher tests passed.\n'
