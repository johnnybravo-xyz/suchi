#!/usr/bin/env bash
#
# Scenario 04: single realistic 100 MB PDF ingest.
#
# The generator's `-mode scan` emits image-XObject pages (~2 MiB grayscale
# each) rather than 6000 lorem pages. This matches what a real 100 MB PDF
# looks like — a scanned document with 30-50 pages — and exercises the
# honest ingest path (qpdf → pdftotext finds no text → OCR route).
#
# The default per-stage timeouts (30s qpdf, 30s pdftotext, 10m OCR) are
# tuned for small PDFs. A realistic 100 MB scan needs headroom on every
# knob or the postingest job dead-letters after 5 retries. We export
# generous overrides via the new SUCHI_*_TIMEOUT / SUCHI_*_MAX_* env vars
# so this scenario measures actual ingest wall-clock, not the retry-to-
# dead cap.
#
# Track the honest number: total wall clock from POST to jobs.state=done,
# plus peak RSS + peak CPU% across the entire window (sampled from /proc).

set -euo pipefail

: "${RESULTS_DIR:?}"
: "${GEN_PDF_BIN:?}"

# Roomy caps for a 100 MB scan-mode PDF. Every value is Go time.ParseDuration
# or a K/M/G byte suffix. Defaults live in core/pipeline/pipeconfig.
export SUCHI_QPDF_TIMEOUT="${SUCHI_QPDF_TIMEOUT:-10m}"
export SUCHI_PDFINSPECTOR_TIMEOUT="${SUCHI_PDFINSPECTOR_TIMEOUT:-10m}"
export SUCHI_ANYDOC_TIMEOUT="${SUCHI_ANYDOC_TIMEOUT:-5m}"
export SUCHI_OCRMYPDF_TIMEOUT="${SUCHI_OCRMYPDF_TIMEOUT:-60m}"
export SUCHI_TESSERACT_TIMEOUT="${SUCHI_TESSERACT_TIMEOUT:-60m}"
export SUCHI_PAGEANALYZE_TIMEOUT="${SUCHI_PAGEANALYZE_TIMEOUT:-10m}"
export SUCHI_QPDF_MAX_SIZE="${SUCHI_QPDF_MAX_SIZE:-500M}"
export SUCHI_OCRMYPDF_MAX_ARCHIVE="${SUCHI_OCRMYPDF_MAX_ARCHIVE:-500M}"

trap bench_teardown EXIT INT TERM

bench_boot_suchi
bench_bootstrap_admin

pdf="$DATA_DIR/big.pdf"
echo "04-single-100mb: generating 100 MB scan-mode PDF at $pdf" >&2
"$GEN_PDF_BIN" -size 100MB -out "$pdf" -mode scan -seed 42
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

echo "04-single-100mb: uploaded doc_id=$doc_id, awaiting postingest (up to 1h)" >&2
# 1 hour cap. OCR of 50 image pages fits comfortably; a stuck stage
# still bounds the scenario.
job_ok=1
bench_wait_job_done "$doc_id" 3600 || job_ok=0

t1_ns="$(date +%s%N)"
elapsed_ms=$(( (t1_ns - t0_ns) / 1000000 ))

bench_stop_sampler

# Best-effort: capture the last recorded stage from the suchi log so the
# report can show which stage dominated. Grepping the log is cheap; we
# don't parse it — just note the final activity marker.
last_stage="$(grep -oE 'post-ingest\.[a-z_.]+' "$SUCHI_LOG" 2>/dev/null | tail -1 || true)"

cat > "$RESULTS_DIR/04-single-100mb.summary.json" <<EOF
{"scenario":"04-single-100mb","kind":"ingest","doc_id":$doc_id,"upload_and_process_ms":$elapsed_ms,"pdf_bytes":$pdf_bytes,"mode":"scan","job_ok":$job_ok,"last_stage":"${last_stage:-unknown}"}
EOF

if [ "$job_ok" -eq 1 ]; then
    echo "04-single-100mb: doc=$doc_id done in ${elapsed_ms}ms pdf=${pdf_bytes}B last_stage=${last_stage:-unknown}" >&2
else
    echo "04-single-100mb: doc=$doc_id DID NOT REACH done in ${elapsed_ms}ms pdf=${pdf_bytes}B last_stage=${last_stage:-unknown}" >&2
fi
