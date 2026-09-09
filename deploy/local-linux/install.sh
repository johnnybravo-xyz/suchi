#!/usr/bin/env bash
set -euo pipefail

fail() { printf 'suchi local: %s\n' "$*" >&2; return 1; }

dependency_help() {
    printf '%s\n' \
        'Install the required packages using your distribution package manager:' \
        '  Debian / Ubuntu: sudo apt install zenity xdg-utils qpdf poppler-utils tesseract-ocr tesseract-ocr-eng imagemagick' \
        '  Fedora: sudo dnf install zenity xdg-utils qpdf poppler-utils tesseract tesseract-langpack-eng ImageMagick' \
        '  Arch: sudo pacman -S zenity xdg-utils qpdf poppler tesseract tesseract-data-eng imagemagick' \
        'Run these commands yourself; Suchi never installs system packages.'
}

check_path() {
    # These paths are embedded in shell, systemd and Desktop Entry files.
    [[ $1 == /* && $1 != *[!a-zA-Z0-9_./\ -]* && $1 != *'/../'* && $1 != */.. ]] ||
        fail "Unsupported path: $1 (use an absolute path containing letters, numbers, spaces, /, ., _, or -)."
}

check_owned_file() {
    [[ ! -L $1 ]] || { fail "Refusing symbolic link: $1"; return 1; }
    if [[ -e $1 ]]; then
        if [[ ! -f $1 || ! -O $1 ]] || ! grep -qx '# Managed by Suchi local installer.' "$1"; then
            fail "Refusing to replace a file not managed by this installer: $1"
            return 1
        fi
    fi
}

dependency_report() {
    # TSV rows are shared by the terminal check and native status table.
    local entry tool purpose status probe_flag missing=0 imagemagick probe languages
    for entry in \
        'systemctl|Keeps Suchi running in your login session' \
        'zenity|Setup dialogs and local settings' \
        'xdg-open|Opens your archive in the browser' \
        'qpdf|Reads and unlocks PDFs' \
        'pdftotext|Extracts text from PDFs' \
        'pdftoppm|Prepares scanned PDF pages for OCR' \
        'pdfinfo|Reads PDF page information' \
        'tesseract|Reads text from scanned documents'; do
        tool=${entry%%|*} purpose=${entry#*|}
        status=Ready
        if ! PATH=/usr/local/bin:/usr/bin:/bin command -v "$tool" >/dev/null; then status=Missing; missing=1; fi
        probe_flag=''
        case "$tool" in qpdf) probe_flag=--version ;; pdftotext|pdftoppm|pdfinfo) probe_flag=-v ;; esac
        if [[ $status == Ready && -n $probe_flag ]] && ! PATH=/usr/local/bin:/usr/bin:/bin "$tool" "$probe_flag" >/dev/null 2>&1; then
            status='Needs attention'; missing=1
            purpose="$purpose; tool could not run"
        fi
        printf '%s\t%s\t%s\n' "$status" "$tool" "$purpose"
    done
    if languages=$(PATH=/usr/local/bin:/usr/bin:/bin tesseract --list-langs 2>/dev/null); then
        status=Ready
        if ! grep -qx eng <<< "$languages"; then status=Missing; missing=1; fi
        printf '%s\tEnglish OCR data (eng)\tReads English text from scans\n' "$status"
    else
        printf 'Needs attention\tEnglish OCR data (eng)\tInstall Tesseract and its English language data\n'
        missing=1
    fi
    imagemagick=$(PATH=/usr/local/bin:/usr/bin:/bin command -v magick || PATH=/usr/local/bin:/usr/bin:/bin command -v convert || true)
    if [[ -z $imagemagick ]]; then
        printf 'Missing\tImageMagick\tCreates PDFs from images\n'
        missing=1
    elif ! probe=$(mktemp -d); then
        printf 'Needs attention\tImageMagick\tCould not create a temporary PDF-writing check\n'
        missing=1
    else
        if printf 'P3\n1 1\n255\n255 255 255\n' > "$probe/input.ppm" &&
            "$imagemagick" "$probe/input.ppm" "PDF:$probe/output.pdf" > "$probe/error" 2>&1 &&
            [[ $(head -c 5 "$probe/output.pdf") == '%PDF-' ]]; then
            printf 'Ready\tImageMagick\tCreates PDFs from images; PDF writing works\n'
        else
            printf 'Needs attention\tImageMagick\tPDF writing failed; review its policy and installation\n'
            missing=1
        fi
        rm -f -- "$probe/input.ppm" "$probe/output.pdf" "$probe/error"
        rmdir -- "$probe"
    fi
    [[ $missing == 0 ]]
}

check_dependencies() {
    local report
    if report=$(dependency_report); then return 0; fi
    printf 'Required local tools need attention:\n%s\n\n' "$report" >&2
    dependency_help >&2
    printf 'ImageMagick must allow PDF writing. Review its policy without broadly enabling PDF reading or unrelated delegates.\n' >&2
    return 1
}

main() (
    local binary='' port='' login=0 uninstall=0 arg
    while [[ $# -gt 0 ]]; do
        arg=$1; shift
        case "$arg" in
            --port) [[ $# -gt 0 ]] || { fail '--port needs a number'; return 1; }; port=$1; shift ;;
            --start-at-login) login=1 ;;
            --no-start-at-login) login=-1 ;;
            --uninstall) uninstall=1 ;;
            --help|-h)
                printf 'Usage: %s /path/to/suchi [--port 8000] [--start-at-login | --no-start-at-login]\n       %s --uninstall\n' "$0" "$0"
                return 0 ;;
            -*) fail "Unknown option: $arg"; return 1 ;;
            *) [[ -z $binary ]] || { fail 'Specify one Suchi binary.'; return 1; }; binary=$arg ;;
        esac
    done
    [[ $(id -u) != 0 ]] || { fail 'Run as your desktop user, without sudo.'; return 1; }
    [[ $(uname -s) == Linux ]] || { fail 'This installer supports Linux with systemd only.'; return 1; }
    umask 077

    local data_base=${XDG_DATA_HOME:-$HOME/.local/share} config_base=${XDG_CONFIG_HOME:-$HOME/.config}
    check_path "$HOME" || return 1
    check_path "$data_base" || return 1
    check_path "$config_base" || return 1
    local local_dir=$data_base/suchi-local program=$data_base/suchi-local/program
    local archive=$data_base/suchi-local/archive config_dir=$config_base/suchi-local
    local config=$config_dir/local-install.toml unit=$config_base/systemd/user/suchi-local.service
    local desktop=$data_base/applications/suchi-local.desktop script_dir
    script_dir=$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)

    local path
    for path in "$data_base" "$config_base" "$local_dir" "$program" "$archive" "$config_dir" "$config_base/systemd" "$config_base/systemd/user" "$data_base/applications"; do
        [[ ! -L $path && ( ! -e $path || ( -d $path && -O $path ) ) ]] || {
            fail "Refusing a symlink or directory not owned by you: $path"; return 1;
        }
    done
    check_owned_file "$unit" || return 1
    check_owned_file "$desktop" || return 1
    if [[ -d $program ]]; then
        [[ -f $program/managed-by-suchi-local && ! -L $program/managed-by-suchi-local ]] || {
            fail "Existing program directory is not managed by this installer: $program"; return 1;
        }
        for path in suchi install.sh launch.sh suchi-tile.svg port; do
            [[ ! -L $program/$path ]] || { fail "Refusing symbolic link: $program/$path"; return 1; }
        done
    fi
    systemctl --user show-environment >/dev/null || {
        fail 'Cannot connect to your systemd user manager. Run inside your desktop login session.'; return 1;
    }
    local unit_state key value load_state='' fragment='' drop_ins=''
    unit_state=$(systemctl --user show --property=LoadState --property=FragmentPath --property=DropInPaths suchi-local.service) || {
        fail 'Could not inspect the existing user service.'; return 1;
    }
    while IFS='=' read -r key value; do
        case "$key" in
            LoadState) load_state=$value ;;
            FragmentPath) fragment=$value ;;
            DropInPaths) drop_ins=$value ;;
        esac
    done <<< "$unit_state"
    if [[ -n $drop_ins || ( -n $fragment && $fragment != "$unit" ) || ( $load_state != not-found && $fragment != "$unit" ) ]]; then
        fail 'Another suchi-local.service or a systemd drop-in already exists. Resolve that service-name conflict before using this installer.'
        return 1
    fi

    if [[ $uninstall == 1 ]]; then
        [[ -z $binary && -z $port && $login == 0 ]] || { fail '--uninstall cannot be combined with install options.'; return 1; }
        [[ -f $program/managed-by-suchi-local && -f $unit ]] || { fail 'No managed local installation found.'; return 1; }
        systemctl --user disable --now suchi-local.service || return 1
        rm -f -- "$unit" "$desktop" "$program/suchi" "$program/install.sh" "$program/launch.sh" "$program/suchi-tile.svg" "$program/port" "$program/managed-by-suchi-local"
        rmdir -- "$program" || printf 'Kept additional files in %s\n' "$program"
        systemctl --user daemon-reload
        printf 'Suchi uninstalled. Your archive, keys and configuration were kept:\n  %s\n  %s\n' "$archive" "$config_dir"
        return 0
    fi

    [[ -n $binary && -f $binary && -x $binary ]] || { fail 'Provide an executable Suchi binary built with local-install support.'; return 1; }
    local binary_help
    binary_help=$(/usr/bin/env -i PATH=/usr/local/bin:/usr/bin:/bin "$binary" help 2>&1) || {
        fail 'The supplied binary could not show Suchi help.'; return 1;
    }
    if [[ $binary_help != *--systemd* || $binary_help != *--setup-token-file* ]]; then
        fail 'This binary lacks local-install support. Build Suchi from the local-install branch.'; return 1;
    fi
    if [[ -z $port && -f $program/port ]]; then port=$(< "$program/port"); fi
    port=${port:-8000}
    if [[ ! $port =~ ^[1-9][0-9]{0,4}$ ]] || (( port < 1024 || port > 65535 )); then
        fail '--port must be an integer between 1024 and 65535.'; return 1;
    fi
    [[ ! -L $config && ( ! -e $config || ( -f $config && -O $config ) ) ]] || {
        fail "Refusing a symlink or configuration not owned by you: $config"; return 1;
    }
    check_dependencies || return 1

    [[ -f $script_dir/install.sh && -f $script_dir/launch.sh ]] || { fail 'Keep install.sh and launch.sh together.'; return 1; }
    local icon=$script_dir/suchi-tile.svg
    if [[ ! -f $icon ]]; then icon=$script_dir/../../core/ui/assets/brand/suchi-tile.svg; fi
    [[ -f $icon ]] || { fail 'The Suchi icon is missing. Extract the complete installer package.'; return 1; }
    mkdir -p -- "$program" "$archive" "$config_dir" "$(dirname -- "$unit")" "$(dirname -- "$desktop")"
    chmod 700 -- "$local_dir" "$program" "$archive" "$config_dir"
    local staged=''
    trap '[[ -z $staged ]] || rm -f -- "$staged"' EXIT
    staged=$(mktemp "$program/.suchi.XXXXXX")
    install -m 700 -- "$binary" "$staged" || return 1
    local active=0
    if [[ -f $unit ]]; then
        if systemctl --user is-active --quiet suchi-local.service; then active=1; fi
        systemctl --user stop suchi-local.service || return 1
    fi
    if [[ ! -e $config ]]; then
        printf '# Optional Suchi boot settings. Configure integrations in the browser.\n# The local launcher pins loopback access and keeps development/demo mode off.\n' > "$config"
    fi
    chmod 600 -- "$config"

    mv -f -- "$staged" "$program/suchi"
    staged=''
    # An installed copy remains available after the download folder is removed.
    if [[ $script_dir != "$program" ]]; then install -m 700 -- "$script_dir/install.sh" "$program/install.sh"; fi
    if [[ $script_dir != "$program" ]]; then install -m 700 -- "$script_dir/launch.sh" "$program/launch.sh"; fi
    if [[ $icon != "$program/suchi-tile.svg" ]]; then install -m 600 -- "$icon" "$program/suchi-tile.svg"; fi
    printf '%s\n' "$port" > "$program/port"
    printf 'Managed by Suchi local installer.\n' > "$program/managed-by-suchi-local"
    printf '%s\n' \
        '# Managed by Suchi local installer.' \
        '[Unit]' \
        'Description=Suchi local document archive' \
        '' \
        '[Service]' \
        'Type=notify' \
        'NotifyAccess=main' \
        'RuntimeDirectory=suchi-local' \
        'RuntimeDirectoryMode=0700' \
        'UMask=0077' \
        "WorkingDirectory=$archive" \
        "ExecStart=/usr/bin/env -i \"HOME=$HOME\" \"PATH=/usr/local/bin:/usr/bin:/bin\" \"NOTIFY_SOCKET=\${NOTIFY_SOCKET}\" \"DATA_DIR=$archive\" \"SUCHI_CONFIG=$config\" \"LISTEN_ADDR=127.0.0.1:$port\" \"PUBLIC_URL=http://127.0.0.1:$port\" \"SUCHI_DEV=0\" \"SUCHI_DEMO_MODE=0\" \"$program/suchi\" serve --systemd \"--setup-token-file=%t/suchi-local/setup-token\"" \
        'Restart=on-failure' \
        'RestartSec=5s' \
        'TimeoutStartSec=120s' \
        'TimeoutStopSec=60s' \
        'NoNewPrivileges=true' \
        '' \
        '[Install]' \
        'WantedBy=default.target' > "$unit"
    printf '%s\n' \
        '# Managed by Suchi local installer.' \
        '[Desktop Entry]' \
        'Type=Application' \
        'Name=Suchi' \
        'Comment=Open your local document archive' \
        "Exec=\"$program/launch.sh\"" \
        "Icon=$program/suchi-tile.svg" \
        'Terminal=false' \
        'Categories=Office;' \
        'Actions=Settings;Stop;' \
        '' \
        '[Desktop Action Settings]' \
        'Name=Settings' \
        "Exec=\"$program/launch.sh\" --settings" \
        '' \
        '[Desktop Action Stop]' \
        'Name=Stop Suchi' \
        "Exec=\"$program/launch.sh\" --stop" > "$desktop"
    systemctl --user daemon-reload
    if [[ $login == 1 ]]; then systemctl --user enable suchi-local.service; fi
    if [[ $login == -1 ]]; then systemctl --user disable suchi-local.service; fi
    if [[ $active == 1 ]]; then systemctl --user start suchi-local.service; fi
    printf 'Installed. Open Suchi from your applications menu, or run:\n  %s\nArchive: %s\nConfiguration: %s\n' "$program/launch.sh" "$archive" "$config"
)

if [[ ${BASH_SOURCE[0]} == "$0" ]]; then main "$@"; fi
