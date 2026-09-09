#!/bin/bash
set -euo pipefail

scripts=$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)
test_dir=$(mktemp -d)
trap 'rm -rf "$test_dir"' EXIT
helpers=$test_dir/helpers
mkdir "$helpers"
# shellcheck source=deploy/local-macos/install.sh
source "$scripts/install.sh"
for tool in qpdf pdftotext pdftoppm pdfinfo launchctl osascript open plutil lsof curl sips iconutil ditto; do
    printf '#!/bin/sh\nexit 0\n' > "$helpers/$tool"
done
printf '#!/bin/sh\nprintf "eng\\n"\n' > "$helpers/tesseract"
# The output helper, not this test process, expands its second argument.
# shellcheck disable=SC2016
printf '#!/bin/sh\nprintf "%%s\\n" "%%PDF-1.7" > "$2"\n' > "$helpers/magick"
chmod +x "$helpers"/*
report=$(check_dependencies "$helpers")
[[ $(printf '%s\n' "$report" | grep -c '^Ready') == 4 ]]
rm "$helpers/qpdf"
if report=$(check_dependencies "$helpers"); then printf 'Accepted missing PDF helper\n' >&2; exit 1; fi
[[ $report == *'Missing — PDF tools'* && $report == *'Installed and ready: pdftotext pdftoppm pdfinfo'* && $report == *'Missing: qpdf'* ]]
printf '#!/bin/sh\nexit 1\n' > "$helpers/pdftotext"
if report=$(check_dependencies "$helpers"); then printf 'Accepted missing and broken PDF helpers\n' >&2; exit 1; fi
[[ $report == *'Missing: qpdf'* && $report == *'Needs attention (installed but could not run): pdftotext'* ]]
printf '#!/bin/sh\nexit 1\n' > "$helpers/qpdf"
chmod +x "$helpers/qpdf"
if report=$(check_dependencies "$helpers"); then printf 'Accepted installed but broken PDF helpers\n' >&2; exit 1; fi
[[ $report == *'Needs attention — PDF tools'* && $report == *'Installed but could not run: qpdf pdftotext'* ]]
printf '#!/bin/sh\nprintf "osd\\n"\n' > "$helpers/tesseract"
if report=$(check_dependencies "$helpers"); then printf 'Accepted missing OCR language data\n' >&2; exit 1; fi
[[ $report == *'English language data (eng) is missing'* ]]
# shellcheck disable=SC2016
printf '#!/bin/sh\nprintf "not a PDF" > "$2"\n' > "$helpers/magick"
if report=$(check_dependencies "$helpers"); then printf 'Accepted invalid conversion output\n' >&2; exit 1; fi
[[ $report == *'Needs attention — Image-to-PDF conversion'* ]]
rm "$helpers/ditto"
if report=$(check_dependencies "$helpers"); then printf 'Accepted missing native helper\n' >&2; exit 1; fi
[[ $report == *'Missing — macOS integration'* && $report == *'Missing system tools: ditto'* && $report == *'Installed: launchctl osascript'* ]]

# Exercise the Finder flow without opening dialogs or invoking a real install.
# shellcheck source=deploy/local-macos/Install.command
source "$scripts/Install.command"
script_dir=$test_dir/bundle
mkdir "$script_dir"
printf '#!/bin/sh\nexit 0\n' > "$script_dir/suchi"
printf '#!/bin/sh\nprintf "simulated install failure\\n" >&2\nexit 42\n' > "$script_dir/install.sh"
chmod +x "$script_dir/suchi" "$script_dir/install.sh"
desktop_home() { printf '%s\n' "$test_dir/user"; }
probe_case=ready
ui_case=ready
check_dependencies() {
    local count
    count=$(< "$test_dir/probes")
    printf '%s\n' "$((count+1))" > "$test_dir/probes"
    if [[ $probe_case == ready || ( $probe_case == recover && $count -gt 0 ) ]]; then printf 'Ready — all document tools\n'; return 0; fi
    printf 'Needs attention — ImageMagick is installed; PDF conversion failed.\n'
    return 1
}
osascript() {
    local source count
    source=$(</dev/stdin)
    if [[ $source == *'with title "Check your computer"'* ]]; then
        count=$(< "$test_dir/check-dialogs")
        printf '%s\n' "$((count+1))" > "$test_dir/check-dialogs"
        printf '%s\n' "$*" >> "$test_dir/reports"
        if [[ $ui_case == cancel ]]; then return 1; fi
        if [[ $ui_case == recover && $count == 0 ]]; then printf 'Check again\n'; else printf 'Continue\n'; fi
    elif [[ $source == *'Start Suchi automatically'* ]]; then
        printf '%s\n' "$*" >> "$test_dir/start-dialog"
        if [[ $ui_case == fail_install ]]; then printf 'Start at login\n'; else return 1; fi
    elif [[ $source == *'Suchi could not be installed'* ]]; then
        printf '%s\n' "$*" > "$test_dir/install-error"
    elif [[ $source == *'Suchi is ready'* ]]; then
        printf 'Unexpected installation success dialog\n' >&2; return 1
    else
        printf 'Unexpected native dialog\n' >&2; return 1
    fi
}
for ui_case in ready cancel recover fail_install; do
    printf '0\n' > "$test_dir/probes"
    printf '0\n' > "$test_dir/check-dialogs"
    probe_case=ready
    [[ $ui_case != cancel ]] || probe_case=broken
    [[ $ui_case != recover ]] || probe_case=recover
    if [[ $ui_case == fail_install ]]; then
        if install_from_finder >/dev/null 2>&1; then printf 'Reported a failed installer as successful\n' >&2; exit 1; fi
        grep -q 'simulated install failure' "$test_dir/install-error"
    else
        install_from_finder >/dev/null
    fi
    expected=1
    [[ $ui_case != recover ]] || expected=2
    [[ $(< "$test_dir/probes") == "$expected" && $(< "$test_dir/check-dialogs") == "$expected" ]]
done
mkdir -p "$test_dir/user/Library/Application Support/Suchi/program"
printf '1\n' > "$test_dir/user/Library/Application Support/Suchi/program/start-at-login"
ui_case=ready
probe_case=ready
install_from_finder >/dev/null
tail -n 1 "$test_dir/start-dialog" | grep -q 'Start at login'
printf 'macOS dependency reports and Finder checks passed.\n'
