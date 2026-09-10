#!/usr/bin/env bash
set -euo pipefail

# Run on an isolated display: xvfb-run -a bash deploy/local-linux/test-dialogs.sh
[[ -n ${DISPLAY:-} ]] || { printf 'A test X display is required; use xvfb-run.\n' >&2; exit 1; }
for tool in zenity xdotool; do
    command -v "$tool" >/dev/null || { printf 'Required dialog test tool is missing: %s\n' "$tool" >&2; exit 1; }
done
export GDK_BACKEND=x11 GSK_RENDERER=cairo
scripts=$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)
test_dir=$(mktemp -d)
worker=''
cleanup() {
    local result=$? dialog_pid=''
    if ((result != 0)); then tail -n 20 "$test_dir/gui.log" "$test_dir/cancel.log" 2>/dev/null || true; fi
    [[ ! -f $test_dir/dialog-pid ]] || dialog_pid=$(< "$test_dir/dialog-pid")
    [[ -z $dialog_pid ]] || kill "$dialog_pid" 2>/dev/null || true
    [[ -z $worker ]] || kill "$worker" 2>/dev/null || true
    [[ -z $worker ]] || wait "$worker" 2>/dev/null || true
    rm -rf -- "$test_dir"
}
trap cleanup EXIT
trap 'exit 130' INT
trap 'exit 143' TERM
prefix=Suchi-test-$$
cp "$scripts/Install.sh" "$scripts/install.sh" "$test_dir/"
printf '#!/bin/sh\nexit 0\n' > "$test_dir/suchi"
chmod +x "$test_dir/suchi"
export XDG_DATA_HOME=$test_dir/data
# The real dialogs run, but installation and service access are forbidden.
# shellcheck source=deploy/local-linux/Install.sh
source "$test_dir/Install.sh"
systemctl() { printf 'Unexpected service access\n' >> "$test_dir/unsafe"; return 1; }
bash() { printf 'Unexpected installation\n' >> "$test_dir/unsafe"; return 1; }
dependency_report() {
    local count=0 status=Missing image_status='Needs attention'
    [[ ! -f $test_dir/probes ]] || count=$(< "$test_dir/probes")
    printf '%s\n' "$((count+1))" > "$test_dir/probes"
    if [[ -f $test_dir/ready ]]; then status=Ready; image_status=Ready; fi
    printf 'Ready\tsystemctl\tKeeps Suchi running in your login session\n%s\tqpdf\tReads and unlocks PDFs\n%s\tEnglish OCR data (eng)\tReads English text from scans\n%s\tImageMagick\tCreates PDFs from images\n' "$status" "$status" "$image_status"
    [[ $status == Ready ]]
}
zenity() {
    local arg pid args=()
    for arg in "$@"; do
        [[ $arg != --title=* ]] || arg="--title=$prefix:${arg#--title=}"
        args+=("$arg")
    done
    command zenity "${args[@]}" <&0 &
    pid=$!
    printf '%s\n' "$pid" > "$test_dir/dialog-pid"
    wait "$pid"
}
window='' previous='' width=0 height=0
wait_dialog() {
    local title=$1 limit=$2 attempt pid='' found='' key value
    for ((attempt=0; attempt<100; attempt++)); do
        [[ ! -f $test_dir/dialog-pid ]] || pid=$(< "$test_dir/dialog-pid")
        if [[ -n $pid ]]; then found=$(xdotool search --onlyvisible --pid "$pid" --name "^$prefix:$title$" 2>/dev/null | head -n 1) || true; fi
        if [[ -n $found && $pid != "$previous" ]]; then window=$found; break; fi
        sleep .1
    done
    [[ -n $found && $pid != "$previous" ]] || { printf 'Dialog did not appear: %s\n' "$title" >&2; return 1; }
    while IFS='=' read -r key value; do
        case "$key" in WIDTH) width=$value ;; HEIGHT) height=$value ;; esac
    done < <(xdotool getwindowgeometry --shell "$window")
    ((width > 0 && width <= limit)) || { printf '%s is too wide: %spx (limit %s).\n' "$title" "$width" "$limit" >&2; return 1; }
    previous=$pid
}
press() { sleep .3; xdotool key --window "$window" "$@"; }

gui_main > "$test_dir/gui.log" 2>&1 & worker=$!
wait_dialog 'Check your computer' 900
# The three equal-width actions are Cancel, Check again, Installation guide.
xdotool mousemove --window "$window" "$((width*5/6))" "$((height-50))" click 1
wait_dialog 'Install document tools' 900
press Return
wait_dialog 'Check your computer' 900
press Return
wait_dialog 'Check your computer' 900
[[ $(< "$test_dir/probes") == 3 ]]
touch "$test_dir/ready"
press Return
wait_dialog 'Check your computer' 900
[[ $(< "$test_dir/probes") == 4 ]]
press Return
wait_dialog 'Install Suchi' 700
press Escape
wait "$worker"; worker=''
[[ ! -e $test_dir/unsafe && ! -e $XDG_DATA_HOME ]]

(if show_dependency_check; then exit 1; fi) > "$test_dir/cancel.log" 2>&1 & worker=$!
wait_dialog 'Check your computer' 900
press Escape
wait "$worker"; worker=''
printf 'Native Linux dependency dialogs, navigation, widths and cancellation passed.\n'
