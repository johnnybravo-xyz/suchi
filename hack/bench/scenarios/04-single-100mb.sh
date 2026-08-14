#!/usr/bin/env bash
#
# Scenario 04: single 100 MB PDF ingest.
# Generate a deterministic 100 MB PDF, upload it, wait for the
# postingest job to reach 'done'. Sampler runs across the full window
# so peak RSS during OCR/chunking is captured.

set -euo pipefail

: "${RESULTS_DIR:?}"
: "${GEN_PDF_BIN:?}"

trap bench_teardown EXIT INT TERM

bench_boot_suchi
bench_bootstrap_admin

pdf="$DATA_DIR/big.pdf"
echo "04-single-100mb: generating 100MB PDF at $pdf" >&2
"$GEN_PDF_BIN" -size 100MB -out "$pdf" -seed 42
pdf_bytes="$(stat -c%s "$pdf")"

jsonl="$RESULTS_DIR/04-single-100mb.jsonl"
bench_start_sampler "$jsonl"

t0_ns="$(date +%s%N)"

resp="$(mktemp)"
http=$(curl -sS -o "$resp" -w '%{http_code}' \
    -X POST "http://127.0.0.1:$SUCHI_PORT/api/documents/" \
    -H "Authorization: Token $ADMIN_TOKEN" \
    -F "document=@$pdf")
if [ "$http" != "200" ] && [ "$http" != "201" ]; then
    echo "04-single-100mb: upload failed http=$http body:" >&2
    cat "$resp" >&2
    rm -f "$resp"
    bench_stop_sampler
    exit 1
fi

doc_id="$(jq -r '.id' < "$resp")"
rm -f "$resp"
if [ -z "$doc_id" ] || [ "$doc_id" = "null" ]; then
    echo "04-single-100mb: response missing .id" >&2
    bench_stop_sampler
    exit 1
fi

echo "04-single-100mb: uploaded doc_id=$doc_id, awaiting postingest" >&2
bench_wait_job_done "$doc_id" 900

t1_ns="$(date +%s%N)"
elapsed_ms=$(( (t1_ns - t0_ns) / 1000000 ))

bench_stop_sampler

cat > "$RESULTS_DIR/04-single-100mb.summary.json" <<EOF
{"scenario":"04-single-100mb","kind":"ingest","doc_id":$doc_id,"upload_and_process_ms":$elapsed_ms,"pdf_bytes":$pdf_bytes}
EOF

echo "04-single-100mb: doc=$doc_id upload+process=${elapsed_ms}ms pdf=${pdf_bytes}B" >&2
