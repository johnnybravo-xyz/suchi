#!/usr/bin/env bash
set -euo pipefail

# Both graphical and command-line installation use the same ownership checks.
# shellcheck source=deploy/local-linux/install.sh
source "$(dirname -- "${BASH_SOURCE[0]}")/install.sh"

show_dependency_check() {
    local report ready status tool purpose button message choice result
    local rows=()
    while :; do
        ready=0
        if report=$(dependency_report); then ready=1; fi
        rows=()
        while IFS=$'\t' read -r status tool purpose; do rows+=("$status" "$tool" "$purpose"); done <<< "$report"
        button='Check again'
        message='Install missing tools or fix the checks below, then choose Check again.\nOpen Installation guide for commands you can run yourself.'
        if [[ $ready == 1 ]]; then
            button=Continue
            message='Your computer is ready for PDFs, images and English OCR.\nContinue to choose how Suchi starts.'
        fi
        choice='' result=0
        choice=$(zenity --list --title='Check your computer' --width=860 --height=530 \
            --text="$message" --column='Status' --column='Tool' --column='Used for' \
            --ok-label="$button" --cancel-label='Cancel' --extra-button='Installation guide' "${rows[@]}") || result=$?
        if [[ $choice == 'Installation guide' ]]; then
            { dependency_help; printf '\nImageMagick must allow writing PDFs. If its check fails, review your distribution policy without broadly enabling PDF reading or unrelated delegates.\n\nAfter installing the tools, return to Check your computer and choose Check again.\n'; } |
                zenity --text-info --title='Install document tools' --width=820 --height=540 --ok-label='Back to checks' || return 1
        elif [[ $result != 0 ]]; then
            return 1
        elif [[ $ready == 1 ]]; then
            return 0
        fi
    done
}

gui_main() {
    local scripts binary choice output program checked=FALSE
    scripts=$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)
    if ! command -v zenity >/dev/null; then
        printf 'Suchi needs Zenity to show the installer. Open a terminal and install the prerequisites, then run Install.sh again.\n' >&2
        dependency_help >&2
        return 1
    fi
    binary=$scripts/suchi
    if [[ ! -f $binary ]]; then binary=$scripts/../../dist/suchi; fi
    if [[ $# != 0 || ! -x $binary ]]; then
        zenity --error --title='Install Suchi' --text='Extract the complete installer archive before opening Install.sh. From a source checkout, run make build first.'
        return 1
    fi
    show_dependency_check || return 0
    program=${XDG_DATA_HOME:-$HOME/.local/share}/suchi-local/program
    if [[ -f $program/managed-by-suchi-local ]] && systemctl --user is-enabled --quiet suchi-local.service; then checked=TRUE; fi
    choice=$(zenity --list --checklist --title='Install Suchi' --width=620 --height=330 \
        --text='Your document archive, on this computer.\n\nSuchi opens in your browser and keeps your documents and keys\nin a private user folder. No Docker or hosting account is needed.\n\nUninstalling keeps your documents and keys.' \
        --column='Enable' --column='Option' --print-column=2 --ok-label='Install' \
        "$checked" 'Start Suchi when I log in') || return 0
    local options=(--no-start-at-login)
    if [[ $choice == 'Start Suchi when I log in' ]]; then options=(--start-at-login); fi
    # A fresh shell preserves install.sh's errexit; calling its sourced main
    # inside this conditional would silently disable failure propagation.
    if ! output=$(bash "$scripts/install.sh" "$binary" "${options[@]}" 2>&1); then
        printf '%s\n' "$output" >&2
        printf '%s\n' "$output" | zenity --text-info --title='Suchi installation could not finish' --width=760 --height=400 --ok-label='Close'
        return 1
    fi
    if zenity --question --title='Suchi is installed' --ok-label='Open Suchi' --cancel-label='Later' \
        --text='Open Suchi to create your account and add your first document. You can also open it from your applications menu. Right-click its shortcut for local settings or to stop it.'; then
        bash "$program/launch.sh"
    fi
}

if [[ ${BASH_SOURCE[0]} == "$0" ]]; then gui_main "$@"; fi
