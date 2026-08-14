#!/usr/bin/env bash
#
# Scenario 05: 1000-doc corpus ingest + list-query latency.
# 1000 deterministic 50KB PDFs, uploaded with concurrency 4, then wait
# for the queue to drain. Sampler covers a 30s steady-state window
# after ingest finishes. Finally, five list-query timings.

set -euo pipefail

: "${RESULTS_DIR:?}"
: "${GEN_PDF_BIN:?}"

trap bench_teardown EXIT INT TERM

bench_boot_suchi
bench_bootstrap_admin

corpus="$DATA_DIR/corpus"
mkdir -p "$corpus"

echo "05-1k-corpus: seeding 1000 x 50KB PDFs into $corpus" >&2
seed_t0="$(date +%s)"
for i in $(seq 1 1000); do
    "$GEN_PDF_BIN" -size 50KB -out "$corpus/doc-$(printf '%04d' "$i").pdf" -seed "$i" >/dev/null
done
seed_t1="$(date +%s)"
seed_time_s=$(( seed_t1 - seed_t0 ))
echo "05-1k-corpus: seed_time_s=$seed_time_s" >&2

upload_one() {
    local file="$1"
    curl -sfS -o /dev/null \
        -X POST "http://127.0.0.1:$SUCHI_PORT/api/documents/" \
        -H "Authorization: Token $ADMIN_TOKEN" \
        -F "document=@$file"
}
export -f upload_one
export SUCHI_PORT ADMIN_TOKEN

echo "05-1k-corpus: uploading with concurrency 4" >&2
find "$corpus" -type f -name '*.pdf' -print0 \
    | xargs -0 -P 4 -n 1 -I {} bash -c 'upload_one "$@"' _ {}

echo "05-1k-corpus: waiting for job queue to drain" >&2
bench_wait_jobs_drain 900

jsonl="$RESULTS_DIR/05-1k-corpus.jsonl"
bench_start_sampler "$jsonl"
echo "05-1k-corpus: 30s steady-state sample" >&2
sleep 30
bench_stop_sampler

# Five list-query timings.
list_query_ms=()
for _ in 1 2 3 4 5; do
    t="$(curl -sfS -o /dev/null -w '%{time_total}\n' \
        "http://127.0.0.1:$SUCHI_PORT/api/documents/?limit=25" \
        -H "Authorization: Token $ADMIN_TOKEN")"
    ms="$(awk -v s="$t" 'BEGIN { printf "%d", s*1000 }')"
    list_query_ms+=("$ms")
done

docs_alive="$(sqlite3 "$DATA_DIR/suchi.db" \
    "SELECT COUNT(*) FROM documents WHERE trashed_at IS NULL")"

# Emit list array in JSON form.
lq_json="$(printf '%s,' "${list_query_ms[@]}")"
lq_json="[${lq_json%,}]"

cat > "$RESULTS_DIR/05-1k-corpus.summary.json" <<EOF
{"scenario":"05-1k-corpus","kind":"corpus","seed_time_s":$seed_time_s,"docs_alive":$docs_alive,"list_query_ms":$lq_json}
EOF

echo "05-1k-corpus: docs_alive=$docs_alive list_query_ms=$lq_json" >&2
