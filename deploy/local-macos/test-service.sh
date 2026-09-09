#!/bin/bash
set -euo pipefail

scripts=$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)
# shellcheck source=deploy/local-macos/common.sh
source "$scripts/common.sh"
[[ $(uname -s) == Darwin && $(id -u) != 0 ]] || { fail 'Run this smoke as a macOS desktop user.'; exit 1; }
[[ $# == 1 && -x $1 ]] || { fail "Usage: $0 /path/to/suchi"; exit 1; }
binary=$(cd -- "$(dirname -- "$1")" && pwd)/$(basename -- "$1")
check_path "$binary"
domain=gui/$(id -u)
launchctl print "$domain" >/dev/null || { fail 'This check needs a macOS graphical login session.'; exit 1; }
test_dir=$(mktemp -d)
test_dir=$(cd "$test_dir" && pwd -P)
label=page.suchi.local-test-$$
target=$domain/$label
plist=$test_dir/$label.plist
port=${PORT:-18782}
if [[ ! $port =~ ^[1-9][0-9]{0,4}$ ]] || (( port < 1024 || port > 65535 )); then fail 'Invalid smoke-test port.'; exit 1; fi
cleanup() {
    local state pid
    if state=$(launchctl print "$target" 2>/dev/null); then
        pid=$(job_pid "$state" "$plist") || return 1
        stop_job "$target" "$pid" || return 1
    fi
    # Only the unique test job's temporary archive and bootstrap data are removed.
    rm -rf "$test_dir"
}
trap cleanup EXIT
trap 'exit 130' INT
trap 'exit 143' TERM
check_path "$test_dir"
umask 077
mkdir "$test_dir/archive" "$test_dir/runtime"
printf '# isolated native service smoke\n' > "$test_dir/config.toml"
printf '%s\n' '<?xml version="1.0" encoding="UTF-8"?>' '<plist version="1.0"><dict>' \
    "<key>Label</key><string>$label</string>" \
    '<key>RunAtLoad</key><true/>' \
    '<key>ProgramArguments</key><array><string>/usr/bin/env</string><string>-i</string>' \
    '<string>PATH=/opt/homebrew/bin:/usr/local/bin:/usr/bin:/bin:/usr/sbin:/sbin</string>' \
    "<string>DATA_DIR=$test_dir/archive</string>" \
    "<string>SUCHI_CONFIG=$test_dir/config.toml</string>" \
    "<string>LISTEN_ADDR=127.0.0.1:$port</string>" \
    "<string>PUBLIC_URL=http://127.0.0.1:$port</string>" \
    "<string>$binary</string><string>serve</string><string>--setup-token-file=$test_dir/runtime/setup-token</string></array>" \
    '<key>Umask</key><integer>63</integer>' \
    "<key>StandardOutPath</key><string>$test_dir/server.log</string>" \
    "<key>StandardErrorPath</key><string>$test_dir/server.log</string>" \
    '</dict></plist>' > "$plist"
plutil -lint "$plist"

wait_until_ready() {
    local state pid attempt
    for ((attempt=0; attempt<60; attempt++)); do
        state=$(launchctl print "$target" 2>/dev/null) || return 1
        pid=$(job_pid "$state" "$plist") || return 1
        if listener_is_owned "$pid" "$port" && curl --noproxy '*' --fail --silent --max-time 2 "http://127.0.0.1:$port/readyz" >/dev/null; then
            printf '%s\n' "$pid"
            return 0
        fi
        sleep 1
    done
    fail 'The owned macOS service did not become ready.'
}

launchctl bootstrap "$domain" "$plist"
first_pid=$(wait_until_ready)
[[ -s $test_dir/runtime/setup-token && $(stat -f '%Lp' "$test_dir/runtime/setup-token") == 600 ]]
if grep -Fq -f "$test_dir/runtime/setup-token" "$test_dir/server.log"; then fail 'Bootstrap token leaked to the server log.'; exit 1; fi
if listener_is_owned "$$" "$port"; then fail 'Accepted a process that does not own the listener.'; exit 1; fi
stop_job "$target" "$first_pid"
if listener_is_owned "$first_pid" "$port"; then fail 'Service retained its listener after shutdown.'; exit 1; fi
launchctl bootstrap "$domain" "$plist"
second_pid=$(wait_until_ready)
[[ $second_pid != "$first_pid" ]]
printf 'Native macOS launchd ownership, private setup, and restart checks passed.\n'
