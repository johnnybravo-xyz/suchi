#!/usr/bin/env bash
#
# Scenario 02: idle RAM.
# Boot suchi with an empty datadir. Wait 5s for GC / init to settle,
# then sample the process for 60s at 100ms cadence. The reporter turns
# this into a steady-state RSS figure.

set -euo pipefail

: "${RESULTS_DIR:?}"

# Each scenario owns its own boot/teardown; the outer bench.sh trap
# isn't inherited into subshells, so we set our own.
trap bench_teardown EXIT INT TERM

bench_boot_suchi
bench_bootstrap_admin

echo "02-idle-ram: warming up 5s" >&2
sleep 5

jsonl="$RESULTS_DIR/02-idle-ram.jsonl"
bench_start_sampler "$jsonl"

echo "02-idle-ram: sampling 60s" >&2
sleep 60

bench_stop_sampler

cat > "$RESULTS_DIR/02-idle-ram.summary.json" <<EOF
{"scenario":"02-idle-ram","kind":"idle","sample_seconds":60}
EOF

echo "02-idle-ram: samples at $jsonl" >&2
