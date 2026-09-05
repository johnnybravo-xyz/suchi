#!/usr/bin/env bash
# End-to-end smoke test of the mail-mbsync recipe.
#
# Drives setup.sh non-interactively with the `generic` provider (dummy
# host, no real IMAP), boots the compose stack, drops synthetic .eml
# fixtures into a temporary recipe, and asserts suchi's fs-watch → post-ingest
# chain produces the expected doc count + attachment fanout + Message-Id
# dedup + parent→child correspondent inheritance.
#
# Not for CI (needs Docker, takes ~90s). For operators to run before
# shipping a change to the recipe. Idempotent — cleans up its own
# scratch state on exit.
#
# Usage:
#   ./smoke-test.sh
#
#   PORT=8765 ./smoke-test.sh     # override port if 8001 is busy

set -euo pipefail

ROOT="$(cd "$(dirname "$0")" && pwd)"
REPO_ROOT="$(cd "$ROOT/../.." && pwd)"

PORT="${PORT:-8001}"
EMAIL="canary@example.com"
PASSWORD="smoke-test-passwd"

# Fail loudly if the port is already taken — otherwise the test hangs
# on healthz forever.
if ss -ltn "sport = :$PORT" | grep -q LISTEN; then
    echo "port $PORT already in use — set PORT=NNNN and retry" >&2
    exit 1
fi

SCRATCH_DIR="$(mktemp -d "${TMPDIR:-/tmp}/suchi-mail-smoke.XXXXXXXX")"
PROJECT="$(basename "$SCRATCH_DIR" | tr '[:upper:].' '[:lower:]-')"
stack_attempted=0

compose() {
    COMPOSE_PROFILES='' SUCHI_BUILD_CONTEXT="$REPO_ROOT" SUCHI_PORT="$PORT" \
        docker compose --project-name "$PROJECT" \
        --project-directory "$SCRATCH_DIR" \
        --file "$SCRATCH_DIR/docker-compose.yml" \
        --env-file "$SCRATCH_DIR/.env" "$@"
}

cleanup() {
    local status=$?
    trap - EXIT
    echo
    echo "== cleanup =="
    if [ "$stack_attempted" -eq 1 ] && ! compose down --rmi local 2>&1 | tail -3; then
        echo "teardown failed; temporary state retained at $SCRATCH_DIR (project $PROJECT)" >&2
        exit 1
    fi
    cd "$ROOT"
    rm -rf -- "$SCRATCH_DIR"
    exit "$status"
}
trap cleanup EXIT

# Copy recipe source only, never existing credentials, mail, or archive data.
cp "$ROOT/docker-compose.yml" "$ROOT/setup.sh" "$SCRATCH_DIR/"
cp -R "$ROOT/mbsync" "$ROOT/bridge" "$SCRATCH_DIR/"
mkdir "$SCRATCH_DIR/templates"
cp "$ROOT/templates/generic.mbsyncrc.tmpl" "$SCRATCH_DIR/templates/"
cd "$SCRATCH_DIR"

echo "== drive setup.sh (generic provider, dummy creds) =="
# option 4 = generic, then host/port/ssl/user/pw/folders/max_msg/max_size/port
printf '4\nimap.invalid\n993\nIMAPS\n%s\ndummy-not-real\nINBOX\n200\n25m\n%s\n' \
    "$EMAIL" "$PORT" | ./setup.sh > /dev/null
echo "  config/.env + .env rendered, template symlinked"

echo
echo "== boot stack =="
stack_attempted=1
compose up --build -d 2>&1 | tail -3

echo
echo "== wait for suchi /healthz =="
for i in $(seq 1 60); do
    if curl -sf "http://127.0.0.1:$PORT/healthz" >/dev/null 2>&1; then
        echo "  healthy after ${i}s"
        break
    fi
    sleep 1
done
curl -sf "http://127.0.0.1:$PORT/healthz" >/dev/null

echo
echo "== bootstrap admin =="
TOKEN=$(compose logs suchi 2>&1 \
    | grep 'localauth.setup.token_minted' \
    | grep -oP '"token":"\K[^"]+' | head -1)
if [ -z "$TOKEN" ]; then
    echo "no setup token in suchi log" >&2
    compose logs suchi 2>&1 | tail -20 >&2
    exit 1
fi
curl -sf -X POST "http://127.0.0.1:$PORT/setup" \
    -H 'Content-Type: application/json' \
    -d "{\"token\":\"$TOKEN\",\"email\":\"$EMAIL\",\"password\":\"$PASSWORD\"}" \
    >/dev/null

echo "  admin created"

echo
echo "== activate fs-watch live =="
curl -sf -X POST "http://127.0.0.1:$PORT/api/login" \
    --cookie-jar "$SCRATCH_DIR/cookies" \
    -H 'Accept: application/json' -H 'Content-Type: application/json' \
    -d "{\"email\":\"$EMAIL\",\"password\":\"$PASSWORD\"}" \
    >/dev/null
curl -sf -X POST "http://127.0.0.1:$PORT/api/admin/settings/ingest" \
    --cookie "$SCRATCH_DIR/cookies" \
    -H 'Sec-Fetch-Site: same-origin' \
    -H 'Content-Type: application/json' \
    -d "{\"fs_watch_dir\":\"/ingest\",\"fs_watch_owner_email\":\"$EMAIL\"}" \
    >/dev/null
for i in $(seq 1 30); do
    if compose logs suchi 2>&1 | grep -q 'fswatch.start'; then
        echo "  fs-watch active after ${i}s without restarting suchi"
        break
    fi
    sleep 1
done
compose logs suchi 2>&1 | grep -q 'fswatch.start'

echo
echo "== drop 9 synthetic fixtures =="
(cd "$REPO_ROOT" && go run ./hack/emlfixtures -out "$SCRATCH_DIR/ingest") 2>&1 | tail -1

# Also drop a HEIC fixture when the host has ImageMagick — the standard
# image ships imagemagick-heic and the HEIC route feeds converted PDFs
# into OCR, so exercising it here proves that whole path end-to-end.
# When magick isn't on the host we skip and drop the HEIC assertion.
heic_expected=0
if command -v magick >/dev/null 2>&1; then
    magick -size 240x240 -background white -fill black \
        -gravity center label:'suchi smoke-test HEIC' \
        ./ingest/canary.heic 2>/dev/null \
        && heic_expected=1 \
        && echo "  + canary.heic dropped"
fi

echo
echo "== wait for docs to stabilize =="
# fs-watch consumes files; post-ingest fans attachments; count peaks
# at 14 (8 emails + 6 attachments) once done — plus 1 for the HEIC if
# it was dropped. Poll until doc count stops changing for 5 samples.
prev=-1
stable=0
for i in $(seq 1 60); do
    docs=$(sqlite3 suchi-data/suchi.db \
        "SELECT COUNT(*) FROM documents WHERE trashed_at IS NULL" 2>/dev/null || echo 0)
    if [ "$docs" = "$prev" ]; then
        stable=$((stable + 1))
        if [ "$stable" -ge 5 ]; then
            echo "  stable at $docs docs for 5 seconds"
            break
        fi
    else
        stable=0
    fi
    prev="$docs"
    sleep 1
done

echo
echo "== assertions =="
FAIL=0
check() {
    local name="$1" got="$2" want="$3"
    if [ "$got" = "$want" ]; then
        printf "  \xE2\x9C\x93 %-45s got=%s\n" "$name" "$got"
    else
        printf "  \xE2\x9C\x97 %-45s got=%s want=%s\n" "$name" "$got" "$want"
        FAIL=1
    fi
}

email_docs=$(sqlite3 suchi-data/suchi.db \
    "SELECT COUNT(*) FROM documents WHERE mime_type='message/rfc822' AND trashed_at IS NULL")
child_docs=$(sqlite3 suchi-data/suchi.db \
    "SELECT COUNT(*) FROM documents WHERE email_parent_id IS NOT NULL AND trashed_at IS NULL")
total_docs=$(sqlite3 suchi-data/suchi.db \
    "SELECT COUNT(*) FROM documents WHERE trashed_at IS NULL")
dup_msgid=$(sqlite3 suchi-data/suchi.db \
    "SELECT COUNT(*) FROM documents WHERE email_message_id='<01-plain@fixtures.suchi>' AND trashed_at IS NULL")
inherited=$(sqlite3 suchi-data/suchi.db \
    "SELECT COUNT(*) FROM document_correspondents dc
     JOIN documents d ON d.id = dc.document_id
     WHERE d.email_parent_id IS NOT NULL AND dc.role = 'sender'")
enc_title=$(sqlite3 suchi-data/suchi.db \
    "SELECT title FROM documents WHERE email_message_id='<04-encoded@fixtures.suchi>'")

dedup_fired=0
compose logs suchi 2>&1 | grep -q 'post-ingest.email.dedup' && dedup_fired=1

check "email docs (parents)"            "$email_docs" "8"
check "attachment child docs"           "$child_docs" "6"
check "total docs alive"                "$total_docs" "$((14 + heic_expected))"
check "dedup: one doc for dup msgID"    "$dup_msgid"  "1"
check "dedup log line fired"            "$dedup_fired" "1"
check "children inherit sender"         "$inherited"   "6"
check "encoded subject decoded"         "$enc_title"   "Statement — 2026"

if [ "$heic_expected" = "1" ]; then
    heic_archived=$(sqlite3 suchi-data/suchi.db \
        "SELECT COUNT(*) FROM documents WHERE mime_type='image/heic' AND archive_blob IS NOT NULL AND trashed_at IS NULL")
    check "HEIC doc has PDF archive"        "$heic_archived" "1"
fi

echo
if [ "$FAIL" -eq 0 ]; then
    echo "ALL ASSERTIONS PASSED"
else
    echo "FAILURES ABOVE. Recent suchi log:"
    compose logs suchi 2>&1 | tail -30
    exit 1
fi
