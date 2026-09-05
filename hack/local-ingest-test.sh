#!/usr/bin/env bash
#
# End-to-end local test of the email ingest chain against synthetic
# .eml fixtures. Boots a scratch suchi against a temp DATA_DIR,
# copies fixtures into INGEST_FS_DIR, waits for the outbox to drain,
# and asserts per-fixture outcomes via curl.
#
# Reset: rm -rf /tmp/suchi-eml-test /tmp/eml-fixtures
#
# Requirements: go, curl, jq, and (optionally) pdftotext + tesseract
# on PATH — attachment OCR is skipped if they're absent, other
# assertions still pass.

set -euo pipefail

ROOT="$(cd "$(dirname "$0")/.." && pwd)"
DATA_DIR="${DATA_DIR:-/tmp/suchi-eml-test/data}"
INGEST_DIR="${INGEST_DIR:-/tmp/suchi-eml-test/ingest}"
FIXTURES="${FIXTURES:-/tmp/eml-fixtures}"
PORT="${PORT:-8765}"
ADMIN_EMAIL="you@example.com"
ADMIN_PASSWORD="local-test-passwd"

mkdir -p "$DATA_DIR" "$INGEST_DIR"

echo "== generate fixtures =="
cd "$ROOT"
go run ./hack/emlfixtures -out "$FIXTURES"

echo
echo "== build suchi =="
make build >/dev/null

echo
echo "== boot suchi =="
PUBLIC_URL="http://127.0.0.1:$PORT" \
LISTEN_ADDR="127.0.0.1:$PORT" \
DATA_DIR="$DATA_DIR" \
INGEST_FS_DIR="$INGEST_DIR" \
INGEST_FS_OWNER_EMAIL="$ADMIN_EMAIL" \
LOG_LEVEL=info \
"$ROOT/dist/suchi" serve > "$DATA_DIR/suchi.log" 2>&1 &
SUCHI_PID=$!
trap 'kill $SUCHI_PID 2>/dev/null || true; wait 2>/dev/null || true' EXIT

# Wait for /healthz.
for i in $(seq 1 40); do
    if curl -sf "http://127.0.0.1:$PORT/healthz" >/dev/null; then break; fi
    sleep 0.25
done
if ! curl -sf "http://127.0.0.1:$PORT/healthz" >/dev/null; then
    echo "suchi never came up:"; tail -50 "$DATA_DIR/suchi.log"; exit 1
fi
echo "suchi up on :$PORT"

echo
echo "== bootstrap admin =="
TOKEN=$(grep 'localauth.setup.token_minted' "$DATA_DIR/suchi.log" | grep -oP '"token":"\K[^"]+' | head -1)
if [ -z "${TOKEN:-}" ]; then
    echo "setup token not found in log"; tail -30 "$DATA_DIR/suchi.log"; exit 1
fi
curl -sf -X POST "http://127.0.0.1:$PORT/setup" \
     -H 'Content-Type: application/json' \
     -d "{\"token\":\"$TOKEN\",\"email\":\"$ADMIN_EMAIL\",\"password\":\"$ADMIN_PASSWORD\"}" \
     >/dev/null

ADMIN_COOKIES="$DATA_DIR/admin.cookies"
curl -sfS -X POST "http://127.0.0.1:$PORT/api/login" \
    --cookie-jar "$ADMIN_COOKIES" \
    -H 'Accept: application/json' -H 'Content-Type: application/json' \
    -d "{\"email\":\"$ADMIN_EMAIL\",\"password\":\"$ADMIN_PASSWORD\"}" >/dev/null
echo "admin created, session acquired"

echo
echo "== activate fs-watch live =="
curl -sf -X POST "http://127.0.0.1:$PORT/api/admin/settings/ingest" \
    --cookie "$ADMIN_COOKIES" -H 'Sec-Fetch-Site: same-origin' \
    -H 'Content-Type: application/json' \
    -d "{\"fs_watch_dir\":\"$INGEST_DIR\",\"fs_watch_owner_email\":\"$ADMIN_EMAIL\"}" \
    >/dev/null

echo
echo "== drop fixtures into ingest =="
cp "$FIXTURES"/*.eml "$INGEST_DIR/"
ls -la "$INGEST_DIR"

echo
echo "== wait for fs-watch to drain the ingest dir =="
# fs-watch deletes files on successful ingest; ingest is complete
# when the dir is empty (modulo errors/, which is a subdir).
for i in $(seq 1 120); do
    remaining=$(find "$INGEST_DIR" -maxdepth 1 -type f -name '*.eml' | wc -l)
    if [ "$remaining" -eq 0 ]; then
        echo "ingest dir drained after ${i}s"
        break
    fi
    sleep 1
done
if [ "$remaining" -ne 0 ]; then
    echo "TIMEOUT: $remaining .eml files still in $INGEST_DIR"; ls "$INGEST_DIR"
    exit 1
fi

echo
echo "== wait for downstream post-ingest to drain =="
# Ingest dir empty means every parent doc was created + post-ingest
# enqueued. Attachment fanout runs inside post-ingest so we can't
# just gate on pending+running=0 (that hits briefly between fanout
# waves). Better signal: doc count stabilizes for N consecutive samples.
prev=-1
stable=0
for i in $(seq 1 60); do
    docs=$(sqlite3 "$DATA_DIR/suchi.db" "SELECT COUNT(*) FROM documents WHERE trashed_at IS NULL")
    if [ "$docs" = "$prev" ]; then
        stable=$((stable + 1))
        if [ "$stable" -ge 5 ]; then
            echo "docs count stable at $docs for 5 seconds"
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
total_docs=$(sqlite3 "$DATA_DIR/suchi.db" \
    "SELECT COUNT(*) FROM documents WHERE trashed_at IS NULL")
email_docs=$(sqlite3 "$DATA_DIR/suchi.db" \
    "SELECT COUNT(*) FROM documents WHERE mime_type='message/rfc822' AND trashed_at IS NULL")
attach_docs=$(sqlite3 "$DATA_DIR/suchi.db" \
    "SELECT COUNT(*) FROM documents WHERE email_parent_id IS NOT NULL AND trashed_at IS NULL")
dup_msgid_count=$(sqlite3 "$DATA_DIR/suchi.db" \
    "SELECT COUNT(*) FROM documents WHERE email_message_id='<01-plain@fixtures.suchi>' AND trashed_at IS NULL")

check() {
    local name="$1" got="$2" want="$3"
    if [ "$got" = "$want" ]; then
        printf "  ✓ %-45s got=%s\n" "$name" "$got"
    else
        printf "  ✗ %-45s got=%s want=%s\n" "$name" "$got" "$want"
        FAIL=1
    fi
}

# 8 unique Message-Ids in the fixture set (fixture 08 dup'd 01's; fix 07 has none).
# unique-msgid parents = 7  +  1 no-msgid parent = 8 total email docs.
check "email docs (parents)" "$email_docs" "8"
# Attachments: 1 (fix02) + 3 (fix05) + 1 real from fix06 + 1 (fix09) = 6
check "attachment child docs"        "$attach_docs" "6"
# Total = 8 parents + 6 children = 14
check "total docs alive"             "$total_docs" "14"
# Duplicate Message-Id must yield ONE doc, not two.
check "dedup: one doc for dup msgID" "$dup_msgid_count" "1"

# Verify the encoded subject decoded correctly.
enc_title=$(sqlite3 "$DATA_DIR/suchi.db" \
    "SELECT title FROM documents WHERE email_message_id='<04-encoded@fixtures.suchi>'")
check "encoded subject decoded" "$enc_title" "Statement — 2026"

# Inline image was NOT ingested as a child doc.
inline_img=$(sqlite3 "$DATA_DIR/suchi.db" \
    "SELECT COUNT(*) FROM documents WHERE mime_type='image/png' AND trashed_at IS NULL")
check "inline image skipped (0 PNG docs)" "$inline_img" "0"

# From address became a correspondent.
bescom_corr=$(sqlite3 "$DATA_DIR/suchi.db" \
    "SELECT COUNT(*) FROM correspondents WHERE name='BESCOM Billing'")
check "from → correspondent upsert" "$bescom_corr" "1"

echo
if [ "$FAIL" -eq 0 ]; then
    echo "ALL ASSERTIONS PASSED"
else
    echo "FAILURES ABOVE. Last 30 log lines:"
    tail -30 "$DATA_DIR/suchi.log"
    exit 1
fi
