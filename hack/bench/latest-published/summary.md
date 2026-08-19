# Suchi v0.1 beta benchmark

Captured 2026-08-18 on Linux 7.1.5 x86-64 with an Intel Core Ultra 7 265U
and 64 GB RAM.

| Scenario | Metric | Value |
| --- | --- | ---: |
| 01 binary size | stripped `dist/suchi` | 27.7 MB |
| 01 binary size | embedded SPA | 0.27 MB |
| 02 idle RAM | median / p95 / max RSS | 36.0 / 36.1 / 36.2 MB |
| 03 cold start | process start to `/healthz` | 87 ms |
| 06 concurrent users | 10 users × 5 uploads | 50/50 successful |
| 06 concurrent users | throughput | 454.5 docs/s |
| 06 concurrent users | p50 / p95 / p99 upload | 5 / 17 / 17 ms |
| 06 concurrent users | peak RSS | 53.3 MB |
| 07 memory profile | idle / post-burst goroutines | 12 / 12 |

Scenarios 01, 02, 03, and 07 passed `make bench-check`; scenario 06 was run
with `USERS=10 DOCS_PER_USER=5`. The burst consisted of generated 200 KB,
13-page text PDFs and measured upload acceptance separately from asynchronous
post-ingest processing.
