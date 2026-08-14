#!/usr/bin/env bash
#
# Scenario 06: concurrent users.
# USERS members, each uploading DOCS_PER_USER ~200KB PDFs sequentially,
# all users in parallel. We capture per-upload latency to derive
# p50/p95/p99 and the wall-clock time to compute throughput.
#
# Env:
#   USERS           default 10
#   DOCS_PER_USER   default 20

set -euo pipefail

: "${RESULTS_DIR:?}"
: "${GEN_PDF_BIN:?}"

USERS="${USERS:-10}"
DOCS_PER_USER="${DOCS_PER_USER:-20}"
TOTAL=$(( USERS * DOCS_PER_USER ))

trap bench_teardown EXIT INT TERM

bench_boot_suchi
bench_bootstrap_admin

# Mint members, collect tokens into a file. We reference tokens by user
# index later, so keep a stable order.
tokens_file="$DATA_DIR/tokens.txt"
: > "$tokens_file"
echo "06-concurrent-users: minting $USERS members" >&2
for u in $(seq 1 "$USERS"); do
    email="member$(printf '%02d' "$u")@bench.local"
    tok="$(bench_mint_member "$email" "bench-passwd-1")"
    if [ -z "$tok" ]; then
        echo "06-concurrent-users: failed to mint $email" >&2
        exit 1
    fi
    printf '%s\n' "$tok" >> "$tokens_file"
done

# Generate DOCS_PER_USER * USERS PDFs at ~200KB, seeds 10001..10001+TOTAL.
pdf_dir="$DATA_DIR/pdfs"
mkdir -p "$pdf_dir"
echo "06-concurrent-users: generating $TOTAL x 200KB PDFs" >&2
for i in $(seq 1 "$TOTAL"); do
    "$GEN_PDF_BIN" -size 200KB -out "$pdf_dir/doc-$(printf '%05d' "$i").pdf" -seed $(( 10000 + i )) >/dev/null
done

jsonl="$RESULTS_DIR/06-concurrent-users.jsonl"
bench_start_sampler "$jsonl"

# Per-user worker: sequential uploads, emitting one JSONL row per upload
# to a per-user results file: {"user":N,"ms":X,"http":C}
lat_dir="$DATA_DIR/lat"
mkdir -p "$lat_dir"

worker() {
    local u="$1" token="$2"
    local out="$lat_dir/user-$u.jsonl"
    : > "$out"
    local i idx pdf http t ms
    for i in $(seq 1 "$DOCS_PER_USER"); do
        idx=$(( (u - 1) * DOCS_PER_USER + i ))
        pdf="$pdf_dir/doc-$(printf '%05d' "$idx").pdf"
        t="$(curl -sS -o /dev/null -w '%{http_code} %{time_total}' \
            -X POST "http://127.0.0.1:$SUCHI_PORT/api/documents/" \
            -H "Authorization: Token $token" \
            -F "document=@$pdf" || echo "000 0")"
        http="${t%% *}"
        secs="${t#* }"
        ms="$(awk -v s="$secs" 'BEGIN { printf "%d", s*1000 }')"
        printf '{"user":%s,"ms":%s,"http":%s}\n' "$u" "$ms" "$http" >> "$out"
    done
}
export -f worker
export DOCS_PER_USER pdf_dir lat_dir SUCHI_PORT

echo "06-concurrent-users: fanning out $USERS parallel uploaders" >&2
wall_t0_ns="$(date +%s%N)"

pids=()
u=0
while IFS= read -r tok; do
    u=$(( u + 1 ))
    ( worker "$u" "$tok" ) &
    pids+=($!)
done < "$tokens_file"

fail=0
for pid in "${pids[@]}"; do
    if ! wait "$pid"; then fail=$(( fail + 1 )); fi
done

wall_t1_ns="$(date +%s%N)"
wall_ms=$(( (wall_t1_ns - wall_t0_ns) / 1000000 ))

echo "06-concurrent-users: uploads done in ${wall_ms}ms, worker failures=$fail" >&2
echo "06-concurrent-users: waiting for jobs to drain" >&2
bench_wait_jobs_drain 600

echo "06-concurrent-users: 30s steady-state sample" >&2
sleep 30
bench_stop_sampler

# Aggregate all per-user JSONL files. Count successes (http 2xx) and errors,
# and compute p50/p95/p99 across all latency samples via awk.
agg="$DATA_DIR/samples.txt"
cat "$lat_dir"/user-*.jsonl > "$DATA_DIR/all.jsonl"

successful="$(grep -oP '"http":2[0-9]{2}' "$DATA_DIR/all.jsonl" | wc -l)"
errors=$(( TOTAL - successful ))

grep -oP '"ms":\K[0-9]+' "$DATA_DIR/all.jsonl" | sort -n > "$agg"
count="$(wc -l < "$agg")"

percentile() {
    local pct="$1"
    awk -v pct="$pct" -v n="$count" '
        BEGIN { idx = int((pct/100.0) * n); if (idx < 1) idx = 1 }
        NR == idx { print; exit }
    ' "$agg"
}

if [ "$count" -gt 0 ]; then
    p50="$(percentile 50)"
    p95="$(percentile 95)"
    p99="$(percentile 99)"
else
    p50=0; p95=0; p99=0
fi

throughput="$(awk -v n="$successful" -v ms="$wall_ms" 'BEGIN { if (ms>0) printf "%.3f", (n*1000.0)/ms; else print "0" }')"

cat > "$RESULTS_DIR/06-concurrent-users.summary.json" <<EOF
{"scenario":"06-concurrent-users","kind":"concurrent","users":$USERS,"docs_per_user":$DOCS_PER_USER,"total_docs":$TOTAL,"successful":$successful,"errors":$errors,"wall_ms":$wall_ms,"throughput_docs_per_s":$throughput,"upload_latency_ms":{"p50":$p50,"p95":$p95,"p99":$p99}}
EOF

echo "06-concurrent-users: successful=$successful errors=$errors wall_ms=$wall_ms p50=$p50 p95=$p95 p99=$p99" >&2
