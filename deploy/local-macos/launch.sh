#!/bin/bash
set -Eeuo pipefail

script_dir=$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)
# shellcheck source=deploy/local-macos/common.sh
source "$script_dir/common.sh"

show_error() {
    trap - ERR
    printf 'suchi local: %s\n' "$1" >&2
    osascript - "$1" <<'APPLESCRIPT' >/dev/null
on run arguments
    display dialog (item 1 of arguments) with title "Suchi" buttons {"OK"} default button "OK" with icon stop
end run
APPLESCRIPT
}

main() {
    local program=$script_dir root runtime port login plist target domain state pid='' attempt ready=0 answer
    root=$(dirname "$program")
    runtime=$root/runtime
    check_private_directory "$root" || return 1
    check_private_directory "$program" || return 1
    check_private_directory "$runtime" || return 1
    for answer in port start-at-login; do
        check_owned_file "$program/$answer" || return 1
        [[ -f $program/$answer ]] || { fail 'Local installation settings are missing. Run the installer again.'; return 1; }
    done
    port=$(< "$program/port")
    login=$(< "$program/start-at-login")
    if [[ ! $port =~ ^[1-9][0-9]{0,4}$ ]] || (( port < 1024 || port > 65535 )); then
        show_error 'Invalid local port. Run the Suchi installer again.'; return 1
    fi
    [[ $login == 0 || $login == 1 ]] || { show_error 'Invalid login preference. Run the Suchi installer again.'; return 1; }
    plist=$program/page.suchi.local.plist
    if [[ $login == 1 ]]; then plist=$(desktop_home)/Library/LaunchAgents/page.suchi.local.plist; fi
    check_managed_file "$plist" || return 1
    domain=gui/$(id -u)
    target=$domain/page.suchi.local
    if state=$(launchctl print "$target" 2>/dev/null); then
        job_pid "$state" "$plist" >/dev/null || return 1
    else
        rotate_log "$runtime" || return 1
        if ! launchctl bootstrap "$domain" "$plist"; then
            # Two concurrent clicks may race to register the same owned job.
            state=$(launchctl print "$target" 2>/dev/null) || {
                show_error 'Could not load Suchi. Check macOS Login Items and the local installation guide.'; return 1;
            }
            job_pid "$state" "$plist" >/dev/null || return 1
        fi
    fi
    if ! launchctl kickstart "$target"; then
        show_error 'Could not start Suchi. Check macOS Login Items and the local installation guide.'; return 1
    fi
    for ((attempt=0; attempt<120; attempt++)); do
        state=$(launchctl print "$target" 2>/dev/null) || break
        pid=$(job_pid "$state" "$plist") || return 1
        if listener_is_owned "$pid" "$port" && curl --noproxy '*' --fail --silent --max-time 2 "http://127.0.0.1:$port/readyz" >/dev/null; then ready=1; break; fi
        sleep 1
    done
    if [[ $ready == 0 ]] || ! listener_is_owned "$pid" "$port"; then
        show_error "Suchi could not open its local port. Another application may be using it. Details: $runtime/server.log"; return 1
    fi
    if [[ -s $runtime/setup-token ]]; then
        check_owned_file "$runtime/setup-token" || return 1
        [[ $(stat -f '%Lp' "$runtime/setup-token") == 600 ]] || { fail 'Setup token is not private (mode 600).'; return 1; }
        # Only the private filename enters argv. The credential is neither an
        # argument, AppleScript source text, browser URL, nor clipboard write.
        answer=$(osascript - "$runtime/setup-token" <<'APPLESCRIPT'
on run arguments
    set setupToken to read (POSIX file (item 1 of arguments)) as «class utf8»
    try
        display dialog "Copy this one-time token, then choose Open Suchi and paste it into the setup form to create your first account." with title "Set up Suchi" default answer setupToken buttons {"Cancel", "Open Suchi"} default button "Open Suchi" cancel button "Cancel"
        return "Open Suchi"
    on error errorMessage number errorNumber
        if errorNumber is -128 then return "Cancel"
        error errorMessage number errorNumber
    end try
end run
APPLESCRIPT
        ) || return 1
        if [[ $answer == Cancel ]]; then return 0; fi
    fi
    if ! open "http://127.0.0.1:$port"; then
        show_error "Could not open your browser. Open http://127.0.0.1:$port manually."; return 1
    fi
}

if [[ ${BASH_SOURCE[0]} == "$0" ]]; then
    trap 'show_error "Suchi could not complete startup. Open Suchi Settings or inspect $(dirname "$script_dir")/runtime/server.log."; exit 1' ERR
    main "$@"
fi
