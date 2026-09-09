#!/usr/bin/env bash
set -euo pipefail

main() {
    local program port runtime
    program=$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)
    port=$(< "$program/port")
    if [[ ! $port =~ ^[1-9][0-9]{0,4}$ ]] || (( port < 1024 || port > 65535 )); then
        zenity --error --text='Suchi has an invalid local port. Run the local installer again.'; return 1;
    fi
    runtime=${XDG_RUNTIME_DIR:?Run Suchi from your desktop login session.}/suchi-local
    # Type=notify succeeds only after this Suchi process has bound its listener.
    if ! systemctl --user start suchi-local.service; then
        zenity --error --text='Suchi could not start. Another application may be using its port. Details: journalctl --user -u suchi-local.service'
        return 1
    fi
    if [[ -s $runtime/setup-token ]]; then
        # Keep the one-time credential out of process arguments and URL history.
        { printf 'Copy this one-time token, then choose Open Suchi and paste it into the setup form to create your first account.\n\n'; cat -- "$runtime/setup-token"; } |
            zenity --text-info --title='Set up Suchi' --width=620 --height=300 --ok-label='Open Suchi' || return 1
    fi
    if ! xdg-open "http://127.0.0.1:$port"; then
        zenity --error --text="Could not open your browser. Open http://127.0.0.1:$port manually."
        return 1
    fi
}

if [[ ${BASH_SOURCE[0]} == "$0" ]]; then main "$@"; fi
