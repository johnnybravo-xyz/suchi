#!/usr/bin/env bash
#
# Scenario 03: cold-start latency.
# Measure wall time from fork() to first successful /healthz.
# We deliberately do NOT use bench_boot_suchi here — that helper waits
# for /healthz internally and swallows the interesting number. Instead
# we inline the boot and time the wait ourselves.

set -euo pipefail

: "${RESULTS_DIR:?}"
: "${SUCHI_BIN:?}"

trap bench_teardown EXIT INT TERM

# Set up the datadir under /tmp/ so the shared teardown can nuke it.
SUCHI_PORT="${SUCHI_PORT:-$PORT}"
DATA_DIR="$(mktemp -d -t suchi-bench-XXXXXXXX)"
case "$DATA_DIR" in
    /tmp/*) : ;;
    *) echo "03-cold-start: refusing DATA_DIR=$DATA_DIR" >&2; exit 1 ;;
esac
SUCHI_LOG="$DATA_DIR/suchi.log"
export SUCHI_PORT DATA_DIR SUCHI_LOG

t0_ns="$(date +%s%N)"

PUBLIC_URL="http://127.0.0.1:$SUCHI_PORT" \
LISTEN_ADDR=":$SUCHI_PORT" \
DATA_DIR="$DATA_DIR" \
LOG_LEVEL=warn \
"$SUCHI_BIN" serve > "$SUCHI_LOG" 2>&1 &
SUCHI_PID=$!
export SUCHI_PID

cold_ms=""
for i in $(seq 1 200); do
    if curl -sfS "http://127.0.0.1:$SUCHI_PORT/healthz" >/dev/null 2>&1; then
        t1_ns="$(date +%s%N)"
        cold_ms=$(( (t1_ns - t0_ns) / 1000000 ))
        break
    fi
    sleep 0.05
done

if [ -z "$cold_ms" ]; then
    echo "03-cold-start: /healthz never came up. tail of log:" >&2
    tail -30 "$SUCHI_LOG" >&2 || true
    exit 1
fi

cat > "$RESULTS_DIR/03-cold-start.summary.json" <<EOF
{"scenario":"03-cold-start","kind":"scalar","label":"cold_start_ms","value":$cold_ms}
EOF

echo "03-cold-start: cold_start_ms=$cold_ms" >&2
