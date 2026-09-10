#!/bin/bash
# Exercise the actual entrypoint traps; sourced functions skip those traps.
# shellcheck disable=SC2329
set -euo pipefail

scripts=$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)
test_dir=$(mktemp -d)
trap 'rm -rf "$test_dir"' EXIT
test_program=$test_dir/Suchi/program
mkdir -p "$test_program" "$test_dir/Suchi/runtime" "$test_dir/commands"
chmod 700 "$test_dir/Suchi" "$test_program" "$test_dir/Suchi/runtime"
cp "$scripts/launch.sh" "$scripts/settings.sh" "$scripts/common.sh" "$test_program/"
printf '18765\n' > "$test_program/port"
printf '0\n' > "$test_program/start-at-login"
printf '<!-- Managed by Suchi local installer. -->\n' > "$test_program/page.suchi.local.plist"
native_os=$(uname -s)
unexpected_failure=0
export test_dir test_program native_os unexpected_failure
export PATH="$test_dir/commands:$PATH"

# Exported mocks are inherited by the child Bash running the real script.
# No command below accesses a real launchd job, browser, or native dialog.
id() { printf '501\n'; }
stat() {
    if [[ $native_os == Darwin ]]; then command stat "$@"; else command stat -c %a "$3"; fi
}
dirname() {
    if [[ $unexpected_failure == 1 && $1 == "$test_program" ]]; then return 1; fi
    command dirname "$@"
}
test_launchctl() {
    case "$1" in
        print)
            [[ -f $test_dir/loaded ]] || return 1
            printf 'path = %s\npid = 12345\n' "$test_program/page.suchi.local.plist"
            ;;
        bootstrap) touch "$test_dir/loaded" ;;
        kickstart) [[ -f $test_dir/loaded ]] ;;
        *) return 1 ;;
    esac
}
test_lsof() {
    if [[ ! -f $test_dir/listener-ready ]]; then touch "$test_dir/listener-ready"; return 1; fi
    printf 'p12345\nn127.0.0.1:18765\n'
}
curl() { :; }
sleep() { :; }
open() { printf '%s\n' "$*" >> "$test_dir/browser"; }
osascript() {
    local script
    printf '%s\n' "$*" >> "$test_dir/dialogs"
    script=$(command cat)
    if [[ $script == *'choose from list'* ]]; then printf 'Stop Suchi\n'; fi
}
export -f id stat dirname test_launchctl test_lsof curl sleep open osascript
# Bash 3.2 handles failing external commands differently from shell functions.
for tool in launchctl lsof; do
    printf '#!/bin/bash\ntest_%s "$@"\n' "$tool" > "$test_dir/commands/$tool"
    chmod 700 "$test_dir/commands/$tool"
done

/bin/bash "$test_program/launch.sh"
[[ $(< "$test_dir/browser") == http://127.0.0.1:18765 ]]
if [[ -e $test_dir/dialogs ]]; then
    printf 'Displayed an error during a normal cold start\n' >&2; exit 1
fi
[[ -e $test_dir/loaded && -e $test_dir/listener-ready ]]
rm "$test_dir/browser" "$test_dir/loaded"

/bin/bash "$test_program/settings.sh"
grep -q 'Suchi is stopped' "$test_dir/dialogs"
if grep -q 'could not complete' "$test_dir/dialogs"; then
    printf 'Displayed an error for an already stopped service\n' >&2; exit 1
fi
[[ ! -e $test_dir/browser ]]

unexpected_failure=1
for entry in launch settings; do
    rm "$test_dir/dialogs"
    if /bin/bash "$test_program/$entry.sh" > "$test_dir/output" 2>&1; then
        printf 'Ignored an unexpected %s failure\n' "$entry" >&2; exit 1
    fi
    [[ ! -e $test_dir/browser ]]
    [[ $(grep -c 'could not complete' "$test_dir/dialogs") == 1 ]]
done
printf 'Local macOS entrypoint error handling tests passed.\n'
