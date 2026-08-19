# Performance guardrails

The accepted v0.1 beta benchmark is
[`hack/bench/latest-published/summary.md`](hack/bench/latest-published/summary.md).
It was measured on 2026-08-18 and 2026-08-19 on an Intel Core Ultra 7 265U.

| Scenario | Metric | Result |
| --- | --- | ---: |
| Binary | Stripped binary | 27.7 MB |
| Idle | Median RSS | 36.0 MB |
| Startup | Process start to `/healthz` | 87 ms |
| Concurrent upload | 10 users x 5 documents | 50/50 successful |
| Concurrent upload | Throughput / p95 | 454.5 docs/s / 17 ms |
| Concurrent upload | Peak RSS | 53.3 MB |
| Hostile mixed reads | 256 clients, 5k-document corpus | 108 requests/s at a 4-connection cap |
| Runtime | Idle / post-burst goroutines | 12 / 12 |

These are measurements from one named host, not universal guarantees. Public
copy rounds them conservatively to a binary under 35 MB and a cold start under
100 ms.

## Reproduce

```sh
make bench-check
USERS=10 DOCS_PER_USER=5 ./hack/bench/bench.sh --scenario 06
```

`make bench-check` rebuilds the release binary and checks binary size, idle
RSS, cold start, and goroutine count against
[`hack/bench/thresholds.json`](hack/bench/thresholds.json). Raw timestamped
runs stay ignored; maintainers copy an accepted report into
`hack/bench/latest-published/`.

Current hard limits are deliberately wider than the published numbers:

| Metric | Target | Hard limit |
| --- | ---: | ---: |
| Idle median RSS | 40 MB | 100 MB |
| Cold start | 100 ms | 1 s |
| Idle goroutines | 15 | 100 |
| Binary size | 30 MB | 60 MB |

## Remaining debt

- The SQLite read pool is capped at four connections. A 2026-08-19 hostile
  benchmark mixed list, FTS search, and preview-metadata reads from 256 clients
  over 5,000 documents. Four connections delivered 108 requests/s at p95
  6.13 s; 8, 16, 32, and unbounded pools were progressively slower under the
  CPU-bound search load, while unbounded opened 256 connections.
- Argon2id still needs 64 MiB while hashing. Suchi returns that memory to the
  OS after each hash, keeping idle RSS low, but setup and login can briefly
  exceed the idle budget.
- QR support initializes tables from `gozxing` even when barcode detection is
  unused. The retained cost is small; address it only if a later profile makes
  it material.
- Scenario 04, scanned 100 MB PDF ingestion, is intentionally outside the fast
  commit guardrail. Run it before releases that change PDF normalization, OCR,
  blank-page analysis, or document splitting.

The original investigation and completed reducer list are preserved in the
workspace archive at `../archive/root/PERF-PLAN.md`.
