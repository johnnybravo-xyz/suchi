# Latest published benchmark artefacts

These are the SVG flame graphs and summary text referenced from the
top-level `PERF-PLAN.md` and `README.md`. They are hand-copied here
from `hack/bench/results/<timestamp>/` after a maintainer accepts
the run. Do NOT edit these files directly — regenerate with
`make bench-check` (or `./hack/bench/bench.sh --scenario 07`), then
copy the desired snapshots over.

The `hack/bench/results/` tree is `.gitignore`-d because it is
large + timestamped; this directory is the small, curated,
tracked snapshot for docs.

## Contents

- `07-flame-idle-inuse.svg` — heap in-use at idle steady-state.
  Interactive: open in a browser, click to zoom, hover for detail.
- `07-flame-idle-alloc.svg` — heap allocation-space at idle (shows
  total churn since start, not just live objects).
- `07-flame-postburst-inuse.svg` — heap in-use after ingesting 10 x
  200 KB PDFs; used to spot allocations retained beyond the burst.
- `07-flame-postburst-alloc.svg` — allocation-space profile after the burst.
- `07-top-inuse-idle.txt` — `go tool pprof -top` text summary of the
  same idle heap.
- `summary.md` — the full report tool output for this run.

## Snapshot

Captured 2026-08-18 during the v0.1 beta audit. Host details and scalar
results are recorded in `summary.md`.
