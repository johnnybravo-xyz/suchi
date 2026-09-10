#!/usr/bin/env bash
set -euo pipefail

show_error() {
    printf 'Suchi: %s\n' "$*" >&2
    if command -v zenity >/dev/null; then zenity --error --no-markup --title='Suchi' --text="$*"; fi
    return 1
}

browser_choice() {
    local force=${2:-0} preference=$1/open-with current=default choice entry name primary fallback candidate checked
    local message='Choose how Suchi opens.\nApp windows use a browser already on this computer, without its\nusual tabs or address bar. Change this any time in Suchi settings.'
    local rows=()
    if [[ -L $preference || ( -e $preference && ( ! -f $preference || ! -O $preference ) ) ]]; then
        show_error 'Suchi cannot use this opening preference file. Remove the unsafe open-with file from its program folder, then choose Open with in Suchi settings.'
        return 1
    fi
    if [[ -f $preference ]]; then current=$(< "$preference"); fi
    case "$current" in
        default|chromium|chromium-browser|google-chrome|google-chrome-stable) ;;
        *)
            if [[ $force != 1 ]]; then
                show_error 'Suchi has an invalid opening preference. Choose Open with in Suchi settings to replace it.'
                return 1
            fi
            current=default ;;
    esac
    if [[ $current != default ]] && ! command -v "$current" >/dev/null; then
        message='The browser previously selected for Suchi is no longer available.\nChoose how to open your archive now.'
        current=default force=1
    fi
    if [[ -f $preference && $force != 1 ]]; then printf '%s\n' "$current"; return 0; fi

    checked=FALSE
    if [[ $current == default ]]; then checked=TRUE; fi
    rows+=("$checked" default 'Browser tab — your default browser')
    for entry in 'Chromium|chromium|chromium-browser' 'Google Chrome|google-chrome|google-chrome-stable'; do
        IFS='|' read -r name primary fallback <<< "$entry"
        candidate=$primary
        if [[ $current == "$fallback" ]] || ! command -v "$primary" >/dev/null; then candidate=$fallback; fi
        if ! command -v "$candidate" >/dev/null; then continue; fi
        checked=FALSE
        if [[ $current == "$candidate" ]]; then checked=TRUE; fi
        rows+=("$checked" "$candidate" "App window — $name")
    done
    if [[ ${#rows[@]} == 3 ]]; then
        if [[ $force != 1 ]]; then printf 'default\n'; return 0; fi
        message="$message\n\nNo supported app-window browser was found. Suchi can still open in your default browser."
    fi
    choice=$(zenity --list --radiolist --title='Open Suchi with' --width=640 --height=350 \
        --text="$message" --column='Use' --column='Browser' --column='Open with' \
        --hide-column=2 --print-column=2 --ok-label='Save choice' "${rows[@]}") || return 1
    # Saved values are browser identifiers, never shell commands or arguments.
    case "$choice" in
        default) ;;
        chromium|chromium-browser|google-chrome|google-chrome-stable)
            command -v "$choice" >/dev/null || { show_error 'That browser is no longer available. Choose Open with in Suchi settings.'; return 1; } ;;
        *) show_error 'No supported opening option was selected.'; return 1 ;;
    esac
    if ! (umask 077; printf '%s\n' "$choice" > "$preference" && chmod 600 -- "$preference"); then
        show_error 'Could not save how Suchi opens. Check that its program folder is writable.'
        return 1
    fi
    printf '%s\n' "$choice"
}

open_browser() {
    local program=$1 url=$2 choice
    choice=$(browser_choice "$program") || return 1
    if [[ $choice == default ]]; then
        if ! xdg-open "$url"; then
            show_error "Could not open your browser. Open $url manually."
            return 1
        fi
    else
        # A fresh browser may stay running; its lifetime must not hold the installer open.
        (
            trap '' HUP
            if ! "$choice" "--app=$url"; then
                show_error "The app-window browser could not open Suchi or closed unexpectedly. Open $url manually, or choose Browser tab under Open with in Suchi settings."
            fi
        ) < /dev/null &
    fi
}

open_archive() {
    local program=$1 port runtime
    port=$(< "$program/port")
    if [[ ! $port =~ ^[1-9][0-9]{0,4}$ ]] || (( port < 1024 || port > 65535 )); then
        show_error 'Suchi has an invalid local port. Run the local installer again.'; return 1;
    fi
    runtime=${XDG_RUNTIME_DIR:?Run Suchi from your desktop login session.}/suchi-local
    # Type=notify succeeds only after this Suchi process has bound its listener.
    if ! systemctl --user start suchi-local.service; then
        show_error 'Suchi could not start. Another application may be using its port. Details: journalctl --user -u suchi-local.service'
        return 1
    fi
    if [[ -s $runtime/setup-token ]]; then
        # Keep the one-time credential out of process arguments and URL history.
        { printf 'Copy this one-time token, then choose Open Suchi and paste it into the setup form to create your first account.\n\n'; cat -- "$runtime/setup-token"; } |
            zenity --text-info --title='Set up Suchi' --width=620 --height=300 --ok-label='Open Suchi' || return 1
    fi
    open_browser "$program" "http://suchi.localhost:$port"
}

stop_archive() {
    local restart='Open Suchi to resume.'
    if ! systemctl --user stop suchi-local.service; then
        show_error 'Could not stop Suchi. Details: journalctl --user -u suchi-local.service'; return 1;
    fi
    if systemctl --user is-enabled --quiet suchi-local.service; then restart='It will resume next time you log in, or when you open Suchi.'; fi
    zenity --info --no-markup --title='Suchi stopped' --text="Your archive is safe. Email intake and processing are paused. $restart"
}

settings() {
    local program=$1 choice output login_action='Start at login'
    if systemctl --user is-enabled --quiet suchi-local.service; then login_action='Do not start at login'; fi
    choice=$(zenity --list --title='Suchi settings' --width=520 --height=350 \
        --text='Manage Suchi on this computer. Archive settings are in the browser.' \
        --column='Action' --ok-label='Continue' \
        'Open Suchi' 'Open with' "$login_action" 'Stop Suchi' 'Uninstall Suchi') || return 0
    case "$choice" in
        'Open Suchi') open_archive "$program" ;;
        'Open with') browser_choice "$program" 1 > /dev/null ;;
        'Start at login')
            systemctl --user enable suchi-local.service || { show_error 'Could not enable login startup.'; return 1; }
            zenity --info --title='Suchi settings' --text='Suchi will start when you log in.' ;;
        'Do not start at login')
            systemctl --user disable suchi-local.service || { show_error 'Could not disable login startup.'; return 1; }
            zenity --info --title='Suchi settings' --text='Suchi will start only when you open it. A running archive has not been stopped.' ;;
        'Stop Suchi') stop_archive ;;
        'Uninstall Suchi')
            zenity --question --no-markup --default-cancel --title='Uninstall Suchi?' --ok-label='Uninstall' \
                --text='This stops Suchi and removes its program and application shortcut. Your documents, keys and configuration are kept. Reinstalling will reopen the same archive.' || return 0
            if ! output=$(bash "$program/install.sh" --uninstall 2>&1); then show_error "$output"; return 1; fi
            printf '%s\n' "$output" | zenity --text-info --title='Suchi uninstalled' --width=620 --height=260 ;;
        '') return 0 ;;
        *) show_error "Unknown settings action: $choice" ;;
    esac
}

main() {
    local program
    program=$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)
    command -v zenity >/dev/null || {
        printf 'Suchi needs Zenity for desktop dialogs. Install zenity with your distribution package manager, then reopen Suchi.\n' >&2
        return 1
    }
    [[ $# -le 1 ]] || { show_error 'Usage: launch.sh [--settings | --stop]'; return 1; }
    case "${1:-}" in
        '') open_archive "$program" ;;
        --settings) settings "$program" ;;
        --stop) stop_archive ;;
        *) show_error 'Usage: launch.sh [--settings | --stop]' ;;
    esac
}

if [[ ${BASH_SOURCE[0]} == "$0" ]]; then main "$@"; fi
