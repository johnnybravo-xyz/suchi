#!/usr/bin/env bash
# Exercise the documented stopped-directory restore, including document bytes.
# Requires a built SUCHI_BIN, curl, jq, and sqlite3. All state stays under /tmp.
set -euo pipefail

# shellcheck source=hack/bench/lib.sh
source "$(dirname "$0")/bench/lib.sh"
SOURCE_DIR=""
cleanup() {
    bench_teardown
    if [ -n "$SOURCE_DIR" ]; then
        case "$SOURCE_DIR" in
            /tmp/suchi-bench-*) rm -rf -- "$SOURCE_DIR" ;;
            *) echo "refusing unexpected source directory: $SOURCE_DIR" >&2; exit 1 ;;
        esac
    fi
}
trap cleanup EXIT
trap 'exit 130' INT
trap 'exit 143' TERM
export BACKUP_INTERVAL=1s BACKUP_KEEP=1
bench_boot_suchi
bench_bootstrap_admin
BASE="http://127.0.0.1:$SUCHI_PORT"

printf '%s\n' 'RESTOREMARKER verifies original bytes and searchable archive content.' > "$DATA_DIR/restore.txt"
DOC_ID="$(curl -fsS "$BASE/api/documents/" \
    -H "Authorization: Token $ADMIN_TOKEN" \
    -F "document=@$DATA_DIR/restore.txt" | jq -er '.id')"
bench_wait_job_done "$DOC_ID" 30

# Preserve the database-only snapshot check alongside the full archive drill.
for _ in $(seq 1 30); do
    snapshots=("$DATA_DIR"/backups/suchi-*.db)
    SNAPSHOT="${snapshots[0]}"
    if [ -f "$SNAPSHOT" ] && [ "$(sqlite3 "$SNAPSHOT" "SELECT count(*) FROM documents WHERE id=$DOC_ID AND content LIKE '%RESTOREMARKER%'")" = 1 ]; then
        break
    fi
    sleep 0.25
done
kill -TERM "$SUCHI_PID"
wait "$SUCHI_PID"
SUCHI_PID=""
snapshots=("$DATA_DIR"/backups/suchi-*.db)
SNAPSHOT="${snapshots[0]}"
test "$(sqlite3 "$SNAPSHOT" 'PRAGMA integrity_check')" = ok
test "$(sqlite3 "$SNAPSHOT" "SELECT count(*) FROM documents WHERE id=$DOC_ID AND content LIKE '%RESTOREMARKER%'")" = 1
test "$(sqlite3 "$SNAPSHOT" 'PRAGMA user_version')" = "$(sqlite3 "$DATA_DIR/suchi.db" 'PRAGMA user_version')"

SOURCE_DIR="$DATA_DIR"
DATA_DIR="$(mktemp -d /tmp/suchi-restore-XXXXXXXX)"
cp -a "$SOURCE_DIR/." "$DATA_DIR/"
cmp "$SOURCE_DIR/.decrypt-key" "$DATA_DIR/.decrypt-key"
test "$(sqlite3 "$DATA_DIR/suchi.db" 'PRAGMA integrity_check')" = ok
test -z "$(sqlite3 "$DATA_DIR/suchi.db" 'PRAGMA foreign_key_check')"
SUCHI_LOG="$DATA_DIR/suchi.log"
ADMIN_COOKIES="$DATA_DIR/admin.cookies"
bench_start_suchi
for _ in $(seq 1 60); do
    if curl -fsS "$BASE/readyz" >/dev/null 2>&1; then break; fi
    if ! kill -0 "$SUCHI_PID" 2>/dev/null; then break; fi
    sleep 0.25
done
curl -fsS "$BASE/readyz" >/dev/null
curl -fsS "$BASE/api/documents/$DOC_ID" \
    -H "Authorization: Token $ADMIN_TOKEN" | jq -e '.content | contains("RESTOREMARKER")' >/dev/null
curl -fsS "$BASE/download/$DOC_ID?raw=1" \
    --cookie "$ADMIN_COOKIES" -o "$DATA_DIR/download.txt"
cmp "$SOURCE_DIR/restore.txt" "$DATA_DIR/download.txt"
curl -fsS "$BASE/api/search/?q=RESTOREMARKER" \
    -H "Authorization: Token $ADMIN_TOKEN" | jq -e --argjson id "$DOC_ID" '.results | any(.id == $id)' >/dev/null
echo 'restore: snapshot integrity, archive integrity, credential key, original download, and search passed'
