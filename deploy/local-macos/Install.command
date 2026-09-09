#!/bin/bash
set -euo pipefail

script_dir=$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)
# shellcheck source=deploy/local-macos/install.sh
source "$script_dir/install.sh"

install_from_finder() {
    local binary=$script_dir/suchi answer details default_start='Only when opened' preference ready
    if [[ ! -x $binary ]]; then
        binary=$(osascript <<'APPLESCRIPT'
POSIX path of (choose file with prompt "Choose the Suchi executable built for this Mac")
APPLESCRIPT
        ) || return 0
    fi
    while true; do
        ready=0
        if details=$(check_dependencies 2>&1); then ready=1; fi
        printf '%s\n' "$details"
        answer=$(osascript - "$details" "$ready" <<'APPLESCRIPT'
on run arguments
    if item 2 of arguments is "1" then
        set result to display dialog ("Your Mac is ready for Suchi." & return & return & item 1 of arguments) with title "Check your computer" buttons {"Cancel", "Check again", "Continue"} default button "Continue" cancel button "Cancel"
    else
        set result to display dialog (item 1 of arguments & return & return & "Install missing document tools with Homebrew, then check again. Installed tools that need attention may need their language data or PDF-write policy fixed.") with title "Check your computer" buttons {"Cancel", "Install guide", "Check again"} default button "Check again" cancel button "Cancel" with icon caution
    end if
    return button returned of result
end run
APPLESCRIPT
        ) || return 0
        if [[ $answer == Continue && $ready == 1 ]]; then break; fi
        if [[ $answer == 'Install guide' ]]; then
            osascript <<'APPLESCRIPT' >/dev/null || return 0
display dialog "If Homebrew is already installed, run this command in Terminal. Suchi does not run it for you. See the installation guide for PDF policy and language-data checks." with title "Install document tools" default answer "brew install qpdf poppler tesseract imagemagick" buttons {"Cancel", "Read guide"} default button "Read guide" cancel button "Cancel"
APPLESCRIPT
            open https://docs.suchi.page/local-install
        fi
    done
    preference=$(desktop_home)/Library/Application\ Support/Suchi/program/start-at-login
    if [[ -f $preference && $(< "$preference") == 1 ]]; then default_start='Start at login'; fi
    answer=$(osascript - "$default_start" <<'APPLESCRIPT'
on run arguments
    return button returned of (display dialog "Start Suchi automatically when you log in to this Mac? It can then process documents and check configured mailboxes even when the browser is closed. You can change this later in Suchi Settings." with title "Install Suchi" buttons {"Cancel", "Only when opened", "Start at login"} default button (item 1 of arguments) cancel button "Cancel")
end run
APPLESCRIPT
    ) || return 0
    local flag=--no-start-at-login
    [[ $answer != 'Start at login' ]] || flag=--start-at-login
    printf 'Installing Suchi for your desktop user…\n'
    if ! details=$("$script_dir/install.sh" "$binary" "$flag" 2>&1); then
        printf '%s\n' "$details" >&2
        osascript - "$details" <<'APPLESCRIPT' >/dev/null
on run arguments
    display dialog (item 1 of arguments) with title "Suchi could not be installed" buttons {"OK"} default button "OK" with icon stop
end run
APPLESCRIPT
        return 1
    fi
    printf '%s\n' "$details"
    osascript <<'APPLESCRIPT' >/dev/null || return 0
display dialog "Suchi is installed in your Applications folder. Open Suchi for your archive; use Suchi Settings to manage startup, stop the app, or uninstall it. Your documents are kept separately from the application." with title "Suchi is ready" buttons {"Later", "Open Suchi"} default button "Open Suchi" cancel button "Later"
APPLESCRIPT
    open "$HOME/Applications/Suchi.app"
}

if [[ ${BASH_SOURCE[0]} == "$0" ]]; then install_from_finder "$@"; fi
