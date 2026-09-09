#!/bin/bash
set -Eeuo pipefail

script_dir=$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)
# shellcheck source=deploy/local-macos/launch.sh
source "$script_dir/launch.sh"

settings() {
    local root program=$script_dir action login target plist state pid message
    root=$(dirname "$program")
    check_private_directory "$root" || return 1
    check_private_directory "$program" || return 1
    check_owned_file "$program/start-at-login" || return 1
    [[ -f $program/start-at-login ]] || { fail 'Local installation settings are missing. Run the installer again.'; return 1; }
    login=$(< "$program/start-at-login")
    if [[ $login == 1 ]]; then action='Turn off start at login'; else action='Start Suchi at login'; fi
    action=$(osascript - "$action" <<'APPLESCRIPT'
on run arguments
    set selectedAction to choose from list {"Open Suchi", item 1 of arguments, "Stop Suchi", "Show archive folder", "Uninstall Suchi"} with title "Suchi Settings" with prompt "Manage the application on this Mac. Archive preferences are in Suchi's browser settings." default items {"Open Suchi"} OK button name "Continue" cancel button name "Cancel"
    if selectedAction is false then return "Cancel"
    return item 1 of selectedAction
end run
APPLESCRIPT
    ) || return 1
    case "$action" in
        'Cancel') return 0 ;;
        'Open Suchi') main ;;
        'Show archive folder') open "$root/archive" ;;
        'Start Suchi at login'|'Turn off start at login')
            local enabled=1
            [[ $login == 0 ]] || enabled=0
            set_login_start "$program" "$enabled" || return 1
            osascript - "$action" <<'APPLESCRIPT' >/dev/null
on run arguments
    display dialog "Login preference saved. You can still open or stop Suchi whenever you need it." with title "Suchi Settings" buttons {"OK"} default button "OK"
end run
APPLESCRIPT
            ;;
        'Stop Suchi')
            target=gui/$(id -u)/page.suchi.local
            plist=$program/page.suchi.local.plist
            if [[ $login == 1 ]]; then plist=$(desktop_home)/Library/LaunchAgents/page.suchi.local.plist; fi
            if state=$(launchctl print "$target" 2>/dev/null); then
                pid=$(job_pid "$state" "$plist") || return 1
                stop_job "$target" "$pid" || return 1
            fi
            message='Suchi is stopped. Intake and processing are paused until you open Suchi again.'
            if [[ $login == 1 ]]; then message="$message It will also start at your next login."; fi
            osascript - "$message" <<'APPLESCRIPT' >/dev/null
on run arguments
    display dialog (item 1 of arguments) with title "Suchi Settings" buttons {"OK"} default button "OK"
end run
APPLESCRIPT
            ;;
        'Uninstall Suchi')
            osascript <<'APPLESCRIPT' >/dev/null || return 0
display dialog "Uninstall Suchi from this Mac? Your documents, database, encryption keys and configuration will be kept. Only the application and its login service are removed." with title "Uninstall Suchi" buttons {"Cancel", "Uninstall"} default button "Cancel" cancel button "Cancel" with icon caution
APPLESCRIPT
            if ! message=$("$program/install.sh" --uninstall 2>&1); then show_error "$message"; return 1; fi
            osascript - "$root" <<'APPLESCRIPT' >/dev/null
on run arguments
    display dialog ("Suchi has been uninstalled. Your archive and configuration remain at:" & return & return & item 1 of arguments) with title "Suchi uninstalled" buttons {"OK"} default button "OK"
end run
APPLESCRIPT
            ;;
        *) show_error 'Unknown local settings action.'; return 1 ;;
    esac
}

if [[ ${BASH_SOURCE[0]} == "$0" ]]; then
    trap 'show_error "Suchi could not complete this action. Check your local installation permissions and try again."; exit 1' ERR
    settings "$@"
fi
