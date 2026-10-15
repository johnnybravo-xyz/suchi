#!/usr/bin/env bash
# SPDX-License-Identifier: AGPL-3.0-or-later
# Real native PDF/image ingestion; no service-manager or desktop changes.
set -euo pipefail

root=$(cd "$(dirname "$0")/.." && pwd)
binary=${1:-$root/dist/suchi}
[[ -x $binary ]] || { echo 'Build Suchi first, or pass its executable path.' >&2; exit 1; }
for tool in curl jq qpdf pdftotext pdftoppm pdfinfo tesseract; do
    command -v "$tool" >/dev/null || { echo "Required: $tool" >&2; exit 1; }
done
converter=$(command -v magick || command -v convert)
smoke_dir=$(mktemp -d "${TMPDIR:-/tmp}/suchi-native-archive.XXXXXXXX")
server_pid=''
base="http://127.0.0.1:${PORT:-18771}"
cleanup() {
    local result=$?
    if [[ -n $server_pid ]]; then
        kill "$server_pid" 2>/dev/null || true
        wait "$server_pid" 2>/dev/null || true
    fi
    if (( result != 0 )) && [[ -f $smoke_dir/server.log ]]; then
        tail -n 40 "$smoke_dir/server.log" >&2 || true
    fi
    rm -rf -- "$smoke_dir"
    return "$result"
}
trap cleanup EXIT
trap 'exit 130' INT
trap 'exit 143' TERM
umask 077
mkdir -m 700 "$smoke_dir/archive" "$smoke_dir/runtime"
printf '\n' > "$smoke_dir/config.toml"
font=DejaVu-Sans
[[ $(uname -s) != Darwin ]] || font=/System/Library/Fonts/Supplemental/Arial.ttf
"$converter" -size 1600x900 xc:white -font "$font" -fill black -pointsize 56 \
    -gravity NorthWest -annotate +80+100 'LOCAL ARCHIVE RECEIPT' \
    -annotate +80+220 'Coffee beans purchase' -annotate +80+340 'Total 425.00' \
    "$smoke_dir/receipt.png"
"$converter" "$smoke_dir/receipt.png" "$smoke_dir/receipt.pdf"

start_server() {
    if curl -s --max-time 1 "$base/healthz" >/dev/null; then
        echo "Another process is using $base; choose another PORT." >&2
        return 1
    fi
    env -i "PATH=$PATH" "HOME=$smoke_dir" "SUCHI_CONFIG=$smoke_dir/config.toml" \
        "DATA_DIR=$smoke_dir/archive" "LISTEN_ADDR=${base#http://}" "PUBLIC_URL=$base" \
        "$binary" serve --setup-token-file="$smoke_dir/runtime/setup-token" \
        >> "$smoke_dir/server.log" 2>&1 &
    server_pid=$!
    for (( attempt=0; attempt<100; attempt++ )); do
        kill -0 "$server_pid" || return 1
        if curl -fsS --max-time 1 "$base/readyz" >/dev/null 2>&1; then return 0; fi
        sleep 0.1
    done
    echo 'Native Suchi did not become ready.' >&2
    return 1
}

start_server
[[ $(curl -sS -o /dev/null -w '%{http_code}' "$base/api/documents/") == 401 ]]
jq -n --rawfile token "$smoke_dir/runtime/setup-token" \
    '{token: ($token | rtrimstr("\n")), email: "native-smoke@example.invalid", password: "native-smoke-password"}' |
    curl -fsS "$base/setup" -H 'Content-Type: application/json' --data-binary @- >/dev/null
[[ ! -e $smoke_dir/runtime/setup-token ]]
printf '%s' '{"email":"native-smoke@example.invalid","password":"native-smoke-password"}' |
    curl -fsS "$base/api/login" -c "$smoke_dir/cookies" \
        -H 'Content-Type: application/json' --data-binary @- >/dev/null

for format in png pdf; do
    doc_id=$(curl -fsS "$base/api/documents/" -b "$smoke_dir/cookies" \
        -H 'Sec-Fetch-Site: same-origin' -F "document=@$smoke_dir/receipt.$format" | jq -er '.id')
    for (( attempt=0; attempt<90; attempt++ )); do
        if curl -fsS "$base/api/documents/$doc_id" -b "$smoke_dir/cookies" |
            jq -e '.content | contains("Coffee beans")' >/dev/null; then break; fi
        sleep 1
    done
    curl -fsS "$base/api/documents/$doc_id" -b "$smoke_dir/cookies" |
        jq -e '.content | contains("Coffee beans")' >/dev/null
    curl -fsS "$base/api/documents/$doc_id/download?raw=1" -b "$smoke_dir/cookies" \
        -o "$smoke_dir/download.$format"
    cmp "$smoke_dir/receipt.$format" "$smoke_dir/download.$format"
done
curl -fsS "$base/api/search/?q=Coffee" -b "$smoke_dir/cookies" | jq -e '.count == 2' >/dev/null
kill "$server_pid"
wait "$server_pid"
server_pid=''
start_server
[[ ! -e $smoke_dir/runtime/setup-token ]]
curl -fsS "$base/api/search/?q=Coffee" -b "$smoke_dir/cookies" | jq -e '.count == 2' >/dev/null
if grep -q '"token":' "$smoke_dir/server.log"; then
    echo 'Bootstrap token appeared in server logs.' >&2
    exit 1
fi
echo 'PASS: native PDF/image OCR, search, original downloads, authentication, and restart persistence'
