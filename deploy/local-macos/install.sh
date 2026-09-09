#!/bin/bash
set -euo pipefail

script_dir=$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)
# shellcheck source=deploy/local-macos/common.sh
source "$script_dir/common.sh"

check_dependencies() {
    local tool executable missing='' broken='' available='' failed=0 imagemagick tesseract languages probe flag
    local service_path=${1:-/opt/homebrew/bin:/usr/local/bin:/usr/bin:/bin:/usr/sbin:/sbin}
    for tool in qpdf pdftotext pdftoppm pdfinfo; do
        executable=$(PATH=$service_path command -v "$tool" || true)
        if [[ -z $executable ]]; then missing="$missing $tool"; continue; fi
        flag=-v
        [[ $tool != qpdf ]] || flag=--version
        if "$executable" "$flag" >/dev/null 2>&1; then available="$available $tool"; else broken="$broken $tool"; fi
    done
    if [[ -n $missing ]]; then
        printf 'Missing — PDF tools\n'
        [[ -z $available ]] || printf '    Installed and ready:%s\n' "$available"
        printf '    Missing:%s\n' "$missing"
        [[ -z $broken ]] || printf '    Needs attention (installed but could not run):%s\n' "$broken"
        failed=1
    elif [[ -n $broken ]]; then
        printf 'Needs attention — PDF tools\n'
        [[ -z $available ]] || printf '    Installed and ready:%s\n' "$available"
        printf '    Installed but could not run:%s\n' "$broken"
        failed=1
    else
        printf 'Ready — PDF tools\n    qpdf, pdftotext, pdftoppm and pdfinfo are installed.\n'
    fi
    tesseract=$(PATH=$service_path command -v tesseract || true)
    if [[ -z $tesseract ]]; then
        printf '\nMissing — Text in scanned documents\n    Install Tesseract and its English language data.\n'
        failed=1
    elif ! languages=$("$tesseract" --list-langs 2>/dev/null); then
        printf '\nNeeds attention — Text in scanned documents\n    Tesseract is installed but could not load its language data.\n'
        failed=1
    elif ! printf '%s\n' "$languages" | grep -qx eng; then
        printf '\nNeeds attention — Text in scanned documents\n    Tesseract is installed; English language data (eng) is missing.\n'
        failed=1
    else
        printf '\nReady — Text in scanned documents\n    Tesseract and English language data are installed.\n'
    fi
    imagemagick=$(PATH=$service_path command -v magick || PATH=$service_path command -v convert || true)
    if [[ -z $imagemagick ]]; then
        printf '\nMissing — Image-to-PDF conversion\n    Install ImageMagick.\n'
        failed=1
    else
        probe=$(mktemp -d) || return 1
        printf 'P3\n1 1\n255\n255 255 255\n' > "$probe/input.ppm"
        if ! "$imagemagick" "$probe/input.ppm" "$probe/output.pdf" > "$probe/error" 2>&1 || [[ ! -s $probe/output.pdf || $(head -c 5 "$probe/output.pdf") != '%PDF-' ]]; then
            printf '\nNeeds attention — Image-to-PDF conversion\n    ImageMagick is installed but could not create a PDF. Check its PDF-write policy.\n'
            failed=1
        else
            printf '\nReady — Image-to-PDF conversion\n    ImageMagick created a PDF successfully.\n'
        fi
        rm -f "$probe/input.ppm" "$probe/output.pdf" "$probe/error"
        rmdir "$probe"
    fi
    missing=''
    available=''
    for tool in launchctl osascript open plutil lsof curl sips iconutil ditto; do
        if PATH=$service_path command -v "$tool" >/dev/null; then available="$available $tool"; else missing="$missing $tool"; fi
    done
    if [[ -n $missing ]]; then
        printf '\nMissing — macOS integration\n    Missing system tools:%s\n' "$missing"
        [[ -z $available ]] || printf '    Installed:%s\n' "$available"
        failed=1
    else
        printf '\nReady — macOS integration\n    Native launcher, background service and icon tools are available.\n'
    fi
    return "$failed"
}

write_app() {
    local app=$1 name=$2 entry=$3 program=$4 user_home=$5 icon=$6
    printf '%s\n' \
        '<?xml version="1.0" encoding="UTF-8"?>' \
        '<!-- Managed by Suchi local installer. -->' \
        '<plist version="1.0"><dict>' \
        "<key>CFBundleIdentifier</key><string>page.suchi.$entry</string>" \
        "<key>CFBundleName</key><string>$name</string>" \
        '<key>CFBundleExecutable</key><string>Suchi</string>' \
        '<key>CFBundlePackageType</key><string>APPL</string>' \
        '<key>CFBundleVersion</key><string>1</string>' \
        '<key>CFBundleIconFile</key><string>Suchi.icns</string>' \
        '<key>LSUIElement</key><true/>' \
        '</dict></plist>' > "$app/Contents/Info.plist" || return 1
    printf '%s\n' '#!/bin/bash' '# Managed by Suchi local installer.' \
        "exec /usr/bin/env -i 'HOME=$user_home' 'PATH=/usr/bin:/bin:/usr/sbin:/sbin' /bin/bash '$program/$entry.sh'" > "$app/Contents/MacOS/Suchi" || return 1
    chmod 700 "$app/Contents/MacOS/Suchi" || return 1
    install -m 600 "$icon" "$app/Contents/Resources/Suchi.icns" || return 1
    plutil -lint "$app/Contents/Info.plist" >/dev/null
}

main() (
    local binary='' port='' login='' uninstall=0 arg
    while [[ $# -gt 0 ]]; do
        arg=$1; shift
        case "$arg" in
            --port) [[ $# -gt 0 ]] || { fail '--port needs a number'; return 1; }; port=$1; shift ;;
            --start-at-login) login=1 ;;
            --no-start-at-login) login=0 ;;
            --uninstall) uninstall=1 ;;
            --help|-h)
                printf 'Usage: %s BINARY [--port 8000] [--start-at-login | --no-start-at-login]\n       %s --uninstall\n' "$0" "$0"
                return 0 ;;
            -*) fail "Unknown option: $arg"; return 1 ;;
            *) [[ -z $binary ]] || { fail 'Specify one Suchi binary.'; return 1; }; binary=$arg ;;
        esac
    done
    [[ $(id -u) != 0 ]] || { fail 'Run as your desktop user, without sudo.'; return 1; }
    [[ $(uname -s) == Darwin ]] || { fail 'This installer supports macOS only.'; return 1; }
    umask 077

    local user_home root program archive config_dir config runtime app settings_app login_plist plist domain target path state pid='' loaded=0 previous_login=0
    user_home=$(desktop_home)
    check_path "$user_home" || return 1
    root=$user_home/Library/Application\ Support/Suchi
    program=$root/program
    archive=$root/archive
    config_dir=$root/config
    config=$config_dir/local-install.toml
    runtime=$root/runtime
    app=$user_home/Applications/Suchi.app
    settings_app=$user_home/Applications/Suchi\ Settings.app
    login_plist=$user_home/Library/LaunchAgents/page.suchi.local.plist
    plist=$program/page.suchi.local.plist
    domain=gui/$(id -u)
    target=$domain/page.suchi.local
    for path in "$user_home" "$user_home/Library" "$user_home/Library/Application Support" "$root" "$program" "$archive" "$config_dir" "$runtime" "$user_home/Applications" "$user_home/Library/LaunchAgents"; do
        check_owned_directory "$path" || return 1
    done
    if [[ -d $program ]]; then
        [[ -f $program/managed-by-suchi-local && ! -L $program/managed-by-suchi-local ]] || {
            fail "Existing program directory is not managed by Suchi: $program"; return 1;
        }
    fi
    local bundle
    for bundle in "$app" "$settings_app"; do
        for path in "$bundle" "$bundle/Contents" "$bundle/Contents/MacOS" "$bundle/Contents/Resources"; do check_owned_directory "$path" || return 1; done
        if [[ -d $bundle && ! -f $bundle/Contents/Info.plist ]]; then
            fail "Existing app is not managed by Suchi: $bundle"; return 1
        fi
        check_managed_file "$bundle/Contents/Info.plist" || return 1
        check_managed_file "$bundle/Contents/MacOS/Suchi" || return 1
        check_owned_file "$bundle/Contents/Resources/Suchi.icns" || return 1
    done
    for path in "$login_plist" "$plist"; do check_managed_file "$path" || return 1; done
    for path in "$config" "$runtime/server.log" "$runtime/setup-token" "$program/suchi" "$program/install.sh" "$program/launch.sh" "$program/common.sh" "$program/settings.sh" "$program/icon.png" "$program/Suchi.icns" "$program/port" "$program/start-at-login" "$program/managed-by-suchi-local"; do check_owned_file "$path" || return 1; done
    if [[ -f $program/start-at-login ]]; then previous_login=$(< "$program/start-at-login"); fi
    [[ $previous_login == 0 || $previous_login == 1 ]] || { fail 'Invalid saved login preference.'; return 1; }
    if [[ $previous_login == 1 ]]; then plist=$login_plist; fi
    launchctl print "$domain" >/dev/null || { fail 'Run inside your macOS desktop login session.'; return 1; }
    if state=$(launchctl print "$target" 2>/dev/null); then
        pid=$(job_pid "$state" "$plist") || return 1
        loaded=1
    fi

    if [[ $uninstall == 1 ]]; then
        [[ -z $binary && -z $port && -z $login ]] || { fail '--uninstall cannot be combined with install options.'; return 1; }
        [[ -f $program/managed-by-suchi-local ]] || { fail 'No managed local installation found.'; return 1; }
        if [[ $loaded == 1 ]]; then stop_job "$target" "$pid" || return 1; fi
        rm -f "$login_plist"
        for bundle in "$app" "$settings_app"; do
            rm -f "$bundle/Contents/MacOS/Suchi" "$bundle/Contents/Info.plist" "$bundle/Contents/Resources/Suchi.icns"
            rmdir "$bundle/Contents/MacOS" "$bundle/Contents/Resources" "$bundle/Contents" "$bundle" 2>/dev/null || printf 'Kept additional files in %s\n' "$bundle"
        done
        rm -f "$program/suchi" "$program/install.sh" "$program/launch.sh" "$program/common.sh" "$program/settings.sh" "$program/icon.png" "$program/Suchi.icns" "$program/port" "$program/start-at-login" "$program/page.suchi.local.plist" "$program/managed-by-suchi-local"
        rmdir "$program" 2>/dev/null || printf 'Kept additional files in %s\n' "$program"
        rm -f "$runtime/setup-token"
        printf 'Suchi uninstalled. Your archive, keys, configuration and logs were kept:\n  %s\n' "$root"
        return 0
    fi

    [[ -n $binary && -f $binary && -x $binary ]] || { fail 'Provide an executable Suchi binary built for this Mac.'; return 1; }
    local binary_help
    binary_help=$(/usr/bin/env -i PATH=/usr/bin:/bin "$binary" help 2>&1) || { fail 'The binary cannot run on this Mac.'; return 1; }
    [[ $binary_help == *--setup-token-file* ]] || { fail 'The binary lacks local-install support.'; return 1; }
    if [[ -z $port && -f $program/port ]]; then port=$(< "$program/port"); fi
    port=${port:-8000}
    if [[ ! $port =~ ^[1-9][0-9]{0,4}$ ]] || (( port < 1024 || port > 65535 )); then
        fail '--port must be an integer between 1024 and 65535.'; return 1
    fi
    login=${login:-$previous_login}
    check_dependencies || return 1
    for path in install.sh launch.sh common.sh settings.sh icon.png; do
        [[ -f $script_dir/$path ]] || { fail 'Keep the installer scripts together.'; return 1; }
    done
    mkdir -p "$program" "$archive" "$config_dir" "$runtime" "$user_home/Library/LaunchAgents" || return 1
    chmod 700 "$root" "$program" "$archive" "$config_dir" "$runtime" || return 1
    printf 'Managed by Suchi local installer.\n' > "$program/managed-by-suchi-local" || return 1
    local staging='' size
    # The only recursive cleanup target is this invocation's mktemp directory.
    trap '[[ -z $staging ]] || rm -rf "$staging"' EXIT
    staging=$(mktemp -d "$program/.stage.XXXXXX") || return 1
    # Preserve Gatekeeper quarantine and other extended attributes; plain
    # install/copy can silently discard that metadata from downloaded binaries.
    ditto --rsrc --extattr --qtn "$binary" "$staging/suchi" || return 1
    chmod 700 "$staging/suchi" || return 1
    for path in install.sh launch.sh common.sh settings.sh; do install -m 700 "$script_dir/$path" "$staging/$path" || return 1; done
    install -m 600 "$script_dir/icon.png" "$staging/icon.png" || return 1
    mkdir "$staging/Suchi.iconset" || return 1
    for size in 16 32 128 256 512; do
        sips -z "$size" "$size" "$staging/icon.png" --out "$staging/Suchi.iconset/icon_${size}x${size}.png" >/dev/null || return 1
        sips -z "$((size*2))" "$((size*2))" "$staging/icon.png" --out "$staging/Suchi.iconset/icon_${size}x${size}@2x.png" >/dev/null || return 1
    done
    iconutil -c icns -o "$staging/Suchi.icns" "$staging/Suchi.iconset" || return 1
    for bundle in Suchi Settings; do mkdir -p "$staging/$bundle/Contents/MacOS" "$staging/$bundle/Contents/Resources" || return 1; done
    write_app "$staging/Suchi" Suchi launch "$program" "$user_home" "$staging/Suchi.icns" || return 1
    write_app "$staging/Settings" 'Suchi Settings' settings "$program" "$user_home" "$staging/Suchi.icns" || return 1
    plist=$staging/page.suchi.local.plist
    printf '%s\n' \
        '<?xml version="1.0" encoding="UTF-8"?>' \
        '<!-- Managed by Suchi local installer. -->' \
        '<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">' \
        '<plist version="1.0"><dict>' \
        '<key>Label</key><string>page.suchi.local</string>' \
        '<key>ProgramArguments</key><array>' \
        '<string>/usr/bin/env</string><string>-i</string>' \
        "<string>HOME=$user_home</string>" \
        '<string>PATH=/opt/homebrew/bin:/usr/local/bin:/usr/bin:/bin:/usr/sbin:/sbin</string>' \
        "<string>DATA_DIR=$archive</string>" \
        "<string>SUCHI_CONFIG=$config</string>" \
        "<string>LISTEN_ADDR=127.0.0.1:$port</string>" \
        "<string>PUBLIC_URL=http://127.0.0.1:$port</string>" \
        '<string>SUCHI_DEV=0</string><string>SUCHI_DEMO_MODE=0</string>' \
        "<string>$program/suchi</string><string>serve</string>" \
        "<string>--setup-token-file=$runtime/setup-token</string>" \
        '</array>' \
        "<key>WorkingDirectory</key><string>$archive</string>" \
        '<key>KeepAlive</key><true/>' \
        '<key>ThrottleInterval</key><integer>10</integer>' \
        '<key>ExitTimeOut</key><integer>60</integer>' \
        '<key>Umask</key><integer>63</integer>' \
        "<key>StandardOutPath</key><string>$runtime/server.log</string>" \
        "<key>StandardErrorPath</key><string>$runtime/server.log</string>" \
        '</dict></plist>' > "$plist" || return 1
    plutil -lint "$plist" >/dev/null || return 1
    if [[ $loaded == 1 ]]; then stop_job "$target" "$pid" || return 1; fi
    if [[ ! -e $config ]]; then
        printf '# Optional Suchi boot settings. Configure integrations in the browser.\n# The local launcher pins loopback access and disables development/demo mode.\n' > "$config" || return 1
    fi
    chmod 600 "$config" || return 1
    for path in suchi install.sh launch.sh common.sh settings.sh icon.png Suchi.icns page.suchi.local.plist; do mv -f "$staging/$path" "$program/$path" || return 1; done
    printf '%s\n' "$port" > "$program/port" || return 1
    printf '%s\n' "$login" > "$program/start-at-login" || return 1
    plist=$program/page.suchi.local.plist
    if [[ $login == 1 ]]; then
        install -m 600 "$plist" "$login_plist" || return 1
        plist=$login_plist
    else
        rm -f "$login_plist" || return 1
    fi
    for bundle in "$app" "$settings_app"; do
        local staged_app=$staging/Suchi
        [[ $bundle != "$settings_app" ]] || staged_app=$staging/Settings
        mkdir -p "$bundle/Contents/MacOS" "$bundle/Contents/Resources" || return 1
        chmod 700 "$bundle" "$bundle/Contents" "$bundle/Contents/MacOS" "$bundle/Contents/Resources" || return 1
        for path in Info.plist MacOS/Suchi Resources/Suchi.icns; do mv -f "$staged_app/Contents/$path" "$bundle/Contents/$path" || return 1; done
    done
    # KeepAlive restarts a launched job. Only opt-in copies enter LaunchAgents,
    # otherwise no job is loaded until the user opens the application.
    if [[ -n $pid ]]; then launchctl bootstrap "$domain" "$plist" || return 1; fi
    printf 'Installed. Open %s\nLocal controls: %s\nArchive: %s\nConfiguration: %s\n' "$app" "$settings_app" "$archive" "$config"
)

if [[ ${BASH_SOURCE[0]} == "$0" ]]; then main "$@"; fi
