# Latest published benchmark evidence

This directory keeps the concise report and allocator table from the accepted
v0.1 run. Raw profiles and timestamped runs are regenerable and ignored.

The `hack/bench/results/` tree is `.gitignore`-d because it is
large + timestamped; this directory is the small, curated,
tracked snapshot for docs.

## Contents

- `07-top-inuse-idle.txt` is the `go tool pprof -top` idle heap summary.
- `summary.md` — the full report tool output for this run.

## Snapshot

Captured 2026-08-21 during the v0.1 beta audit. Host details and scalar
results are recorded in `summary.md`.
