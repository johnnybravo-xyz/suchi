#!/usr/bin/env bash
#
# Scenario 07: heap + goroutine profile at steady-state.
# Requires SUCHI_PPROF=1 (exported below) and `go` on PATH.
# Captures /debug/pprof/heap and /debug/pprof/goroutine twice — once at
# idle steady-state after warmup, once after a 10-doc x 200KB ingest
# burst — so the delta between the two isolates ingest-path allocations
# from the always-on baseline.

set -euo pipefail

: "${RESULTS_DIR:?}"
: "${GEN_PDF_BIN:?}"

trap bench_teardown EXIT INT TERM

# lib.sh's bench_boot_suchi inherits the current shell env, so exporting
# SUCHI_PPROF here is enough to enable the pprof endpoints in the child.
export SUCHI_PPROF=1

bench_boot_suchi
bench_bootstrap_admin

echo "07-mem-profile: warming 5s" >&2
sleep 5

heap_idle="$RESULTS_DIR/07-heap-idle.pprof"
goro_idle="$RESULTS_DIR/07-goroutine-idle.txt"
echo "07-mem-profile: capturing idle heap + goroutine snapshots" >&2
curl -sf "http://127.0.0.1:$SUCHI_PORT/debug/pprof/heap" -o "$heap_idle"
curl -sf "http://127.0.0.1:$SUCHI_PORT/debug/pprof/goroutine?debug=1" -o "$goro_idle"

burst_dir="$DATA_DIR/burst"
mkdir -p "$burst_dir"
echo "07-mem-profile: generating 10 x 200KB PDFs" >&2
for i in $(seq 20001 20010); do
    "$GEN_PDF_BIN" -size 200KB -out "$burst_dir/doc-$i.pdf" -seed "$i" >/dev/null
done

echo "07-mem-profile: uploading burst" >&2
for f in "$burst_dir"/*.pdf; do
    curl -sfS -o /dev/null \
        -X POST "http://127.0.0.1:$SUCHI_PORT/api/documents/" \
        -H "Authorization: Token $ADMIN_TOKEN" \
        -F "document=@$f"
done

echo "07-mem-profile: waiting for jobs to drain" >&2
bench_wait_jobs_drain "${JOB_DRAIN_TIMEOUT:-300}"

heap_post="$RESULTS_DIR/07-heap-postburst.pprof"
goro_post="$RESULTS_DIR/07-goroutine-postburst.txt"
echo "07-mem-profile: capturing post-burst heap + goroutine snapshots" >&2
curl -sf "http://127.0.0.1:$SUCHI_PORT/debug/pprof/heap" -o "$heap_post"
curl -sf "http://127.0.0.1:$SUCHI_PORT/debug/pprof/goroutine?debug=1" -o "$goro_post"

# Top-10 tables. `head -25` keeps the pprof header + top 10 rows with
# some breathing room; go tool pprof prints ~13 header lines before the
# ranked list.
go tool pprof -top -sample_index=inuse_space -unit=mb "$heap_idle" 2>/dev/null \
    | head -25 > "$RESULTS_DIR/07-top-inuse-idle.txt"
go tool pprof -top -sample_index=alloc_space -unit=mb "$heap_idle" 2>/dev/null \
    | head -25 > "$RESULTS_DIR/07-top-alloc-idle.txt"
go tool pprof -top -sample_index=inuse_space -unit=mb "$heap_post" 2>/dev/null \
    | head -25 > "$RESULTS_DIR/07-top-inuse-postburst.txt"
go tool pprof -top -sample_index=alloc_space -unit=mb "$heap_post" 2>/dev/null \
    | head -25 > "$RESULTS_DIR/07-top-alloc-postburst.txt"

goroutines_idle="$(head -1 "$goro_idle" | sed -n 's/.*total \([0-9][0-9]*\).*/\1/p')"
goroutines_postburst="$(head -1 "$goro_post" | sed -n 's/.*total \([0-9][0-9]*\).*/\1/p')"

# Interactive flame graphs. Soft-skips if perl or the vendored pieces are
# missing (bench_render_flame handles the guard) so scenario 07 still yields
# top-N tables even on hosts without the flame toolchain.
bench_build_flame_deps
flame_idle_inuse="$RESULTS_DIR/07-flame-idle-inuse.svg"
flame_idle_alloc="$RESULTS_DIR/07-flame-idle-alloc.svg"
flame_post_inuse="$RESULTS_DIR/07-flame-postburst-inuse.svg"
flame_post_alloc="$RESULTS_DIR/07-flame-postburst-alloc.svg"
bench_render_flame "$heap_idle" "$flame_idle_inuse" inuse_space "idle heap (in-use)"
bench_render_flame "$heap_idle" "$flame_idle_alloc" alloc_space "idle heap (alloc-space)"
bench_render_flame "$heap_post" "$flame_post_inuse" inuse_space "post-burst heap (in-use)"
bench_render_flame "$heap_post" "$flame_post_alloc" alloc_space "post-burst heap (alloc-space)"

cat > "$RESULTS_DIR/07-mem-profile.summary.json" <<EOF
{"scenario":"07-mem-profile","kind":"profile","goroutines_idle":$goroutines_idle,"goroutines_postburst":$goroutines_postburst,"heap_idle_path":"07-heap-idle.pprof","heap_postburst_path":"07-heap-postburst.pprof","flame_idle_inuse_path":"07-flame-idle-inuse.svg","flame_idle_alloc_path":"07-flame-idle-alloc.svg","flame_postburst_inuse_path":"07-flame-postburst-inuse.svg","flame_postburst_alloc_path":"07-flame-postburst-alloc.svg"}
EOF

echo "07-mem-profile: goroutines idle=$goroutines_idle postburst=$goroutines_postburst" >&2
