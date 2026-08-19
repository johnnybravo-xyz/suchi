# Suchi benchmark harness

This directory contains repeatable Linux benchmarks for one Suchi binary. Raw
runs are written to ignored timestamped directories; accepted release evidence
lives in `latest-published/`.

## Guardrail check

```sh
make bench-check
```

This rebuilds Suchi and checks binary size, idle memory, cold start, and
goroutine count against `thresholds.json`.

| Metric | Target | Hard limit |
| --- | ---: | ---: |
| Idle median RSS | 40 MB | 100 MB |
| Cold start | 100 ms | 1 s |
| Idle goroutines | 15 | 100 |
| Binary size | 30 MB | 60 MB |

The hard limits catch regressions. Public claims must use an accepted measured
run, not these limits.

## Full harness

```sh
make build
./hack/bench/bench.sh
./hack/bench/bench.sh --scenario 06
```

Use `--keep` to retain a scenario's temporary data directory. Scenario 06
accepts `USERS` and `DOCS_PER_USER`; for example:

```sh
USERS=10 DOCS_PER_USER=5 ./hack/bench/bench.sh --scenario 06
```

| Scenario | Measures |
| --- | --- |
| 01 | Stripped binary and embedded SPA size |
| 02 | Idle RSS, CPU, and threads |
| 03 | Process start to `/healthz` |
| 04 | One 100 MB PDF ingest and peak RSS |
| 05 | 1,000-document ingest and API reads |
| 06 | Concurrent upload latency and throughput |
| 07 | Heap, goroutines, top allocators, and flame graphs |

## Accepted v0.1 snapshot

The current report is [`latest-published/summary.md`](latest-published/summary.md),
measured on 2026-08-18 and 2026-08-19 on an Intel Core Ultra 7 265U.

| Metric | Result |
| --- | ---: |
| Stripped binary | 27.7 MB |
| Idle median RSS | 36.0 MB |
| Cold start | 87 ms |
| 10 users x 5 uploads | 50/50 successful |
| Upload throughput / p95 | 454.5 documents/s / 17 ms |
| Upload peak RSS | 53.3 MB |
| Idle / post-burst goroutines | 12 / 12 |

These are measurements from one host, not universal guarantees. Record
`uname -a`, the CPU model, and total memory beside any newly accepted run.

## Profiling output

Scenario 07 emits heap profiles, allocator tables, and interactive SVG flame
graphs. Inspect a profile directly with:

```sh
go tool pprof -http=:0 hack/bench/results/<timestamp>/07-heap-idle.pprof
```

The SVG renderer uses Brendan Gregg's `flamegraph.pl`, vendored with its
CDDL-1.0 license under `tools/flamegraph/`. Without Perl, the scenario still
produces pprof files and allocator tables.

## Known costs

- The SQLite read pool is capped at four connections. Higher caps performed
  worse in the recorded CPU-bound mixed-search benchmark.
- Argon2id briefly uses 64 MiB during password hashing, above idle memory.
- QR support initializes `gozxing` tables even when barcode detection is off.
- Scenario 04 is intentionally outside the fast commit guardrail. Run it for
  changes to PDF normalization, OCR, blank-page analysis, or splitting.

The harness reads Linux `/proc`, so its memory and process measurements are not
portable to macOS or Windows. Each scenario owns a temporary directory and
process, and teardown verifies the temporary path before removal.
