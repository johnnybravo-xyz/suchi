# Suchi v0.1 beta benchmark

Captured 2026-08-22 on Linux 7.1.5 x86-64 with an Intel Core Ultra 7 265U
and 64 GB RAM.

| Scenario | Metric | Value |
| --- | --- | ---: |
| 01 binary size | stripped `dist/suchi` | 25.5 MB |
| 01 binary size | embedded SPA | 0.29 MB |
| 02 idle RAM | median / p95 / max RSS | 31.9 / 31.9 / 31.9 MB |
| 03 cold start | process start to `/healthz` | 86 ms |
| 06 concurrent users | 10 users × 20 uploads | 200/200 successful |
| 06 concurrent users | upload acceptance | 625 requests/s |
| 06 concurrent users | p50 / p95 / p99 upload | 3 / 10 / 23 ms |
| 06 concurrent users | peak RSS | 50.8 MB |
| 07 memory profile | idle / post-burst goroutines | 12 / 12 |

Scenarios 01, 02, 03, and 07 passed the benchmark thresholds; scenario 06 was
run with `USERS=10 DOCS_PER_USER=20`. The burst consisted of generated 200 KB,
13-page text PDFs and measured upload acceptance separately from asynchronous
post-ingest processing.
