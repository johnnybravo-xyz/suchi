#!/usr/bin/env bash
# SPDX-License-Identifier: AGPL-3.0-or-later
#
# End-to-end smoke for retained ASN QR splitting, UI-facing projections,
# writable metadata conflicts, and completed-batch upload replay.

set -euo pipefail

ROOT="$(cd "$(dirname "$0")/.." && pwd)"
SCRATCH_DIR="$(mktemp -d "${TMPDIR:-/tmp}/suchi-asn-smoke.XXXXXX")"
DATA_DIR="$SCRATCH_DIR/data"
INGEST_DIR="$SCRATCH_DIR/ingest"
FIXTURE="$SCRATCH_DIR/asn-batch.pdf"
PORT="${PORT:-8767}"
URL="http://127.0.0.1:$PORT"
ADMIN_EMAIL="asn-smoke@example.com"
ADMIN_PASSWORD="asn-smoke-password"
SUCHI_PID=""

cleanup() {
    if [ -n "$SUCHI_PID" ]; then
        kill "$SUCHI_PID" 2>/dev/null || true
        wait "$SUCHI_PID" 2>/dev/null || true
    fi
    chmod -R u+w "$SCRATCH_DIR" 2>/dev/null || true
    rm -rf -- "$SCRATCH_DIR"
}
trap cleanup EXIT
trap 'exit 130' INT
trap 'exit 143' TERM

fail() {
    printf 'asn-smoke: %s\n' "$1" >&2
    printf '%s\n' '--- server log ---' >&2
    tail -60 "$DATA_DIR/suchi.log" >&2 || true
    exit 1
}

for tool in go curl jq sqlite3 pdftoppm qpdf tesseract; do
    command -v "$tool" >/dev/null 2>&1 || {
        printf 'asn-smoke: required tool not found: %s\n' "$tool" >&2
        exit 1
    }
done
if curl -s --max-time 1 "$URL/healthz" >/dev/null 2>&1; then
    fail "$URL is already serving; choose another PORT"
fi

mkdir -p "$DATA_DIR" "$INGEST_DIR"

printf '%s\n' '== generate retained-label feeder PDF =='
cd "$ROOT"
go run ./hack/asnfixture -out "$FIXTURE" -first 4021 -second 4022
pages="$(qpdf --show-npages "$FIXTURE")"
[ "$pages" = 4 ] || fail "fixture has $pages pages, want 4"

printf '%s\n' '== build and boot Suchi with ASN processing off =='
make build >/dev/null
PUBLIC_URL="$URL" \
LISTEN_ADDR="127.0.0.1:$PORT" \
SUCHI_DEV=0 \
SUCHI_DEMO_MODE=0 \
OIDC_ISSUER_URL='' \
LLM_ENDPOINT_URL='' \
DATA_DIR="$DATA_DIR" \
INGEST_FS_DIR="$INGEST_DIR" \
INGEST_FS_OWNER_EMAIL="$ADMIN_EMAIL" \
OCR_ENGINE=tesseract \
LOG_LEVEL=info \
"$ROOT/dist/suchi" serve >"$DATA_DIR/suchi.log" 2>&1 &
SUCHI_PID=$!

for _ in $(seq 1 80); do
    if curl -fsS --max-time 1 "$URL/healthz" >/dev/null 2>&1 &&
       curl -fsS --max-time 1 "$URL/readyz" >/dev/null 2>&1; then
        break
    fi
    kill -0 "$SUCHI_PID" 2>/dev/null || fail 'server exited during startup'
    sleep 0.25
done
curl -fsS --max-time 1 "$URL/readyz" >/dev/null 2>&1 || fail 'server did not become ready'

printf '%s\n' '== bootstrap and activate watched-folder intake =='
TOKEN="$(sed -n 's/.*"token":"\([^"]*\)".*/\1/p' "$DATA_DIR/suchi.log" | sed -n '1p')"
[ -n "$TOKEN" ] || fail 'setup token not found'
curl -fsS -X POST "$URL/setup" \
    -H 'Content-Type: application/json' \
    -d "{\"token\":\"$TOKEN\",\"email\":\"$ADMIN_EMAIL\",\"password\":\"$ADMIN_PASSWORD\"}" \
    >/dev/null

COOKIES="$DATA_DIR/admin.cookies"
curl -fsS -X POST "$URL/api/login" \
    --cookie-jar "$COOKIES" \
    -H 'Accept: application/json' -H 'Content-Type: application/json' \
    -d "{\"email\":\"$ADMIN_EMAIL\",\"password\":\"$ADMIN_PASSWORD\"}" \
    >/dev/null

printf '%s\n' '== enable ASN splitting through Advanced settings =='
curl -fsS -X POST "$URL/api/admin/settings/scan" \
    --cookie "$COOKIES" \
    -H 'Sec-Fetch-Site: same-origin' -H "Origin: $URL" \
    -H 'Content-Type: application/json' \
    -d '{"scan_asn_mode":"split","scan_asn_prefix":"ASN","scan_split_dpi":300}' \
    >/dev/null
scan_settings="$(curl -fsS --cookie "$COOKIES" "$URL/api/admin/settings/scan")"
[ "$(jq -r '.scan_asn_mode' <<<"$scan_settings")" = split ] ||
    fail 'live ASN mode did not reload'
[ "$(jq -r '.scan_asn_prefix' <<<"$scan_settings")" = ASN ] ||
    fail 'live ASN prefix did not reload'
[ "$(jq -r '.scan_split_dpi' <<<"$scan_settings")" = 300 ] ||
    fail 'live scan DPI did not reload'

settings_body="$(jq -nc --arg dir "$INGEST_DIR" --arg owner "$ADMIN_EMAIL" \
    '{fs_watch_dir:$dir,fs_watch_owner_email:$owner}')"
curl -fsS -X POST "$URL/api/admin/settings/ingest" \
    --cookie "$COOKIES" \
    -H 'Sec-Fetch-Site: same-origin' -H "Origin: $URL" \
    -H 'Content-Type: application/json' \
    -d "$settings_body" >/dev/null

cp "$FIXTURE" "$INGEST_DIR/asn-batch.pdf"

printf '%s\n' '== wait for atomic fanout =='
origin_id=''
for _ in $(seq 1 120); do
    origin_id="$(sqlite3 "$DATA_DIR/suchi.db" \
        "SELECT split_origin_id FROM documents WHERE archive_serial_number IN (4021,4022) AND trashed_at IS NULL GROUP BY split_origin_id HAVING COUNT(*)=2 LIMIT 1;")"
    [ -n "$origin_id" ] && break
    kill -0 "$SUCHI_PID" 2>/dev/null || fail 'server exited during fanout'
    sleep 0.5
done
[ -n "$origin_id" ] || fail 'two ASN children were not committed'

parent_trashed="$(sqlite3 "$DATA_DIR/suchi.db" \
    "SELECT COUNT(*) FROM documents WHERE id=$origin_id AND trashed_at IS NOT NULL;")"
[ "$parent_trashed" = 1 ] || fail 'combined parent is not in Trash'
[ -f "$INGEST_DIR/asn-batch.pdf" ] || fail 'read-only watcher removed its source PDF'

child_ids="$(sqlite3 "$DATA_DIR/suchi.db" \
    "SELECT group_concat(id, ',') FROM (SELECT id FROM documents WHERE split_origin_id=$origin_id AND trashed_at IS NULL ORDER BY split_index);")"
IFS=',' read -r child_one child_two <<<"$child_ids"
[ -n "${child_one:-}" ] && [ -n "${child_two:-}" ] || fail 'child IDs unavailable'

children_json="$(curl -fsS --cookie "$COOKIES" \
    "$URL/api/documents/?split_origin_id=$origin_id&page_size=200")"
child_count="$(jq -r '.count' <<<"$children_json")"
child_asns="$(jq -r '[.results[].archive_serial_number] | sort | map(tostring) | join(",")' <<<"$children_json")"
[ "$child_count" = 2 ] || fail "split projection count=$child_count, want 2"
[ "$child_asns" = '4021,4022' ] || fail "split projection ASNs=$child_asns"

for child in "$child_one" "$child_two"; do
    child_pdf="$SCRATCH_DIR/child-$child.pdf"
    curl -fsS --cookie "$COOKIES" "$URL/api/documents/$child/download" -o "$child_pdf"
    child_pages="$(qpdf --show-npages "$child_pdf")"
    [ "$child_pages" = 2 ] || fail "child $child has $child_pages pages, want retained marker plus content"
done

printf '%s\n' '== exercise writable ASN validation and conflicts =='
patch_json="$SCRATCH_DIR/patch.json"
status="$(curl -sS -o "$patch_json" -w '%{http_code}' -X PATCH \
    --cookie "$COOKIES" -H 'Sec-Fetch-Site: same-origin' -H "Origin: $URL" \
    -H 'Content-Type: application/json' -d '{"archive_serial_number":4022}' \
    "$URL/api/documents/$child_two")"
[ "$status" = 200 ] || fail "same-value ASN retry returned $status"

status="$(curl -sS -o "$patch_json" -w '%{http_code}' -X PATCH \
    --cookie "$COOKIES" -H 'Sec-Fetch-Site: same-origin' -H "Origin: $URL" \
    -H 'Content-Type: application/json' -d '{"archive_serial_number":4023}' \
    "$URL/api/documents/$child_two")"
[ "$status" = 200 ] || fail "ASN reassignment returned $status"

status="$(curl -sS -o "$patch_json" -w '%{http_code}' -X PATCH \
    --cookie "$COOKIES" -H 'Sec-Fetch-Site: same-origin' -H "Origin: $URL" \
    -H 'Content-Type: application/json' -d '{"archive_serial_number":4023}' \
    "$URL/api/documents/$child_one")"
conflict_code="$(jq -r '.code // empty' "$patch_json")"
[ "$status" = 409 ] && [ "$conflict_code" = archive_serial_number_conflict ] ||
    fail "retained ASN conflict returned status=$status code=$conflict_code"

status="$(curl -sS -o "$patch_json" -w '%{http_code}' -X PATCH \
    --cookie "$COOKIES" -H 'Sec-Fetch-Site: same-origin' -H "Origin: $URL" \
    -H 'Content-Type: application/json' -d '{"archive_serial_number":4022}' \
    "$URL/api/documents/$child_two")"
[ "$status" = 200 ] || fail "ASN restoration returned $status"

printf '%s\n' '== re-upload completed batch without resurrecting parent =='
replay_json="$SCRATCH_DIR/replay.json"
status="$(curl -sS -o "$replay_json" -w '%{http_code}' -X POST \
    --cookie "$COOKIES" -H 'Sec-Fetch-Site: same-origin' -H "Origin: $URL" \
    -F "document=@$FIXTURE;filename=asn-batch-retry.pdf" \
    "$URL/api/documents/")"
replay_id="$(jq -r '.id // 0' "$replay_json")"
replay_dedup="$(jq -r '.deduplicated // false' "$replay_json")"
[ "$status" = 200 ] && [ "$replay_id" = "$origin_id" ] && [ "$replay_dedup" = true ] ||
    fail "completed-batch replay returned status=$status id=$replay_id deduplicated=$replay_dedup"

parent_trashed="$(sqlite3 "$DATA_DIR/suchi.db" \
    "SELECT COUNT(*) FROM documents WHERE id=$origin_id AND trashed_at IS NOT NULL;")"
children_live="$(sqlite3 "$DATA_DIR/suchi.db" \
    "SELECT COUNT(*) FROM documents WHERE split_origin_id=$origin_id AND trashed_at IS NULL;")"
[ "$parent_trashed" = 1 ] && [ "$children_live" = 2 ] ||
    fail "replay changed split state: parent_trashed=$parent_trashed children_live=$children_live"

printf 'asn-smoke: passed origin=%s children=%s,%s ASNs=4021,4022\n' \
    "$origin_id" "$child_one" "$child_two"
