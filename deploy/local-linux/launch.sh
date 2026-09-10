#!/usr/bin/env bash
set -euo pipefail

show_error() {
    printf 'Suchi: %s\n' "$*" >&2
    if command -v zenity >/dev/null; then zenity --error --no-markup --title='Suchi' --text="$*"; fi
    return 1
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
    if ! xdg-open "http://suchi.localhost:$port"; then
        show_error "Could not open your browser. Open http://suchi.localhost:$port manually."
        return 1
    fi
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
        'Open Suchi' "$login_action" 'Stop Suchi' 'Uninstall Suchi') || return 0
    case "$choice" in
        'Open Suchi') open_archive "$program" ;;
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
