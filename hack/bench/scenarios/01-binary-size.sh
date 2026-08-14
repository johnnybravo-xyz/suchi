#!/usr/bin/env bash
#
# Scenario 01: binary size.
# No boot required. We stat the shipped binary and the embedded SPA
# bundle. Both are strong signals for the "single-binary, no runtime"
# story we tell in the README.

set -euo pipefail

# Bench lib is already sourced by bench.sh's per-scenario subshell.
: "${RESULTS_DIR:?RESULTS_DIR must be exported by bench.sh}"
: "${SUCHI_BIN:?SUCHI_BIN must be exported by bench.sh}"
: "${REPO_ROOT:?REPO_ROOT must be exported by bench.sh}"

if [ ! -f "$SUCHI_BIN" ]; then
    echo "01-binary-size: $SUCHI_BIN missing" >&2
    exit 1
fi

bin_bytes="$(stat -c%s "$SUCHI_BIN")"
spa_bytes=0
if [ -d "$REPO_ROOT/core/ui/spa/dist" ]; then
    spa_bytes="$(du -sb "$REPO_ROOT/core/ui/spa/dist" | cut -f1)"
fi

out="$RESULTS_DIR/01-binary-size.summary.json"
cat > "$out" <<EOF
{"scenario":"01-binary-size","kind":"binary-size","artifacts":[{"name":"dist/suchi","bytes":$bin_bytes},{"name":"core/ui/spa/dist","bytes":$spa_bytes}]}
EOF

echo "01-binary-size: dist/suchi=$bin_bytes bytes, spa/dist=$spa_bytes bytes" >&2
