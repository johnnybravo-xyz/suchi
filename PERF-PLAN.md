# Suchi perf plan — cut idle RAM from 162 MB → sub-100 MB

Snapshot: 2026-08-14 · commit `2765120` (pre-perf-work) · host CachyOS
7.1.5-1 · Go 1.22 · argon2id defaults `m=64MiB, t=2, p=2`.

## Measured baseline (`hack/bench/`)

| Scenario | Metric | Value |
|---|---|---|
| 01 binary size | `dist/suchi` (stripped, `-trimpath -ldflags=-s -w`) | **27.0 MB** |
| 01 embed | `core/ui/spa/dist/` | 253 KB |
| 02 idle RAM | `VmRSS` median (60 s sample) | **161.7 MB** |
| 02 idle RAM | `VmRSS` min → max | 161.5 → 161.9 MB (flat) |
| 02 idle CPU | median → p95 → max | 0.0 → 0.0 → 140 % (single-tick spike) |
| 03 cold start | boot → first `/healthz` 200 | **69 ms** |
| 04 100 MB PDF | ingest outcome | **postingest reached `dead` at 300 s per-job cap** (6427-page lorem PDF exceeds OCR budget) |
| 04 100 MB PDF | peak `VmRSS` / peak CPU during window | **505 MB / 2660 %** (~26 cores pinned) |
| 06 10 users × 5 docs (200 KB) | wall + throughput + errors | **97 ms, 515 docs/s, 0/50 errors** |
| 06 10 users × 5 docs (200 KB) | upload latency p50 / p95 / p99 | **5 / 11 / 13 ms** |
| 06 10 users × 5 docs (200 KB) | peak `VmRSS` during burst + postingest | **45.9 MB** (only ~10 MB above idle) |
| 07 heap idle | in-use total | 67.5 MB |
| 07 heap idle | argon2 tail | **64 MB (95 % of in-use)** |
| 07 heap post-burst | in-use total | 4.5 MB (argon2 released after GC) |
| 07 goroutines idle | count | 12 |
| 07 goroutines post-burst | count | 12 (no leak) |

**162 MB idle vs the ~100 MB claim** = ~60 % over budget. The gap is
almost entirely the argon2id password-hash working set — 64 MB is
allocated once at admin bootstrap, released to Go's heap, but never
returned to the OS. The 95 MB delta between Go's 67 MB in-use and
the process's 162 MB RSS is Go runtime overhead: mmap arenas held
by the allocator for reuse.

## Reducers, ranked by MB saved / LOC changed

### R1 — force `debug.FreeOSMemory()` after every argon2 call · ~30 LOC · **LANDED · -129 MB (161.7 → 32.7 MB idle)**

The argon2 working set is 64 MiB of `[]block`, freed at Go level as
soon as `IDKey` returns. Go retains the mmap until it decides the
kernel needs it back — which never happens on a lightly loaded box.
`runtime/debug.FreeOSMemory()` forces a GC and calls `madvise` on
freed spans.

- **Where:** `plugins/local-auth/localauth.go` — wrap `argon2.IDKey`
  in `HashPassword` (line 317) and `VerifyPassword` (line 343) so both
  paths trigger the release.
- **Cost:** 10–30 ms extra latency on setup/login. Login latency is
  already dominated by argon2 (~200 ms) — a 15 % bump is fine.
- **Risk:** low. `FreeOSMemory` is stable stdlib.

### R2 — set `GOMEMLIMIT` in `runServe` bootstrap · 5 LOC · est. **-20 to -40 MB** steady-state RSS

`GOMEMLIMIT` (Go 1.19+) tells the runtime a soft ceiling; the GC
targets returning above that threshold instead of doubling.

- **Where:** `distro/cmd/suchi/main.go` near `runServe` top. If the
  env is unset, default to `128MiB` — override via env.
- **Cost:** more GC cycles under high load; sometimes 5–10 % higher
  CPU during ingest. Idle is untouched.
- **Risk:** low. Documented Go tunable.

### R3 — drop argon2 memory param 64 → 32 MiB · 1 LOC · est. **-32 MB** peak, no idle change

Halves the peak working set during hashing. NIST SP 800-63B recommends
argon2id with ≥ 15 MiB; 32 MiB is comfortably safe for interactive
auth on a home server. Existing hashes stay verifiable — encoded form
carries `m=`, so old rows verify at their original cost.

- **Where:** `plugins/local-auth/localauth.go:304` — flip constant.
- **Cost:** modest security downgrade (still ~30× above NIST floor).
- **Risk:** low; verifiable via existing `TestPasswordRoundTrip`.

### R4 — stream `pageanalyze.meanWhitePGM` · ~40 LOC · est. **-8 to -10 MB** peak during ingest

`meanWhitePGM` reads the full PGM into memory (9.5 MB alloc per page
observed in the burst profile). PGM is a header + raw byte stream —
scanning row-by-row halves peak.

- **Where:** `core/pipeline/pageanalyze/pageanalyze.go`.
- **Cost:** ~5 % more syscalls per page. Wall clock unchanged.
- **Risk:** medium — this is on the postingest hot path. Requires
  the existing pageanalyze test corpus to stay green.

### R5 — investigate SQLite prepared-statement cache · exploratory · est. **-5 to -15 MB** idle

`modernc.org/sqlite.interruptOnDone.func1` holds 1 MB idle in `inuse`.
The prepared-statement cache is per connection; on a 4-CPU box that
scales. Check `core/db.Open` for `SetMaxOpenConns` sizing.

- **Where:** `core/db/db.go`.
- **Cost:** none if we already cap to 1 writer connection.
- **Risk:** low.

### R6 — audit `gozxing/reedsolomon.NewGenericGF` init · 1 LOC · est. **-0.5 MB**

Package-init allocs 520 KB of Galois-field tables even if we never
scan a QR code. Only used by the QR-detect path in post-ingest.
Move the init into a lazy `sync.Once`.

- **Where:** dependency; upstream patch or fork. Low priority.

### R7 — audit unused `runtime/metrics.init.0` · exploratory · est. **-0.5 MB**

500 KB of runtime metric descriptors. Unavoidable if we use metrics
at all. Skip.

**Sum of R1-R5 if all landed: -125 to -170 MB against a 162 MB baseline.**
Even R1 alone is expected to get us to ~90 MB idle.

## Workflow — keep RAM in check every commit

### 1. `make bench-check` — the guardrail target

Add to `Makefile`:

```make
bench-check: build
	@./hack/bench/bench.sh --scenario 02 --scenario 03 --scenario 07 --thresholds
```

### 2. Thresholds in `hack/bench/lib.sh`

New function `bench_check_thresholds` reads `hack/bench/thresholds.json`
and fails non-zero if any budget is exceeded:

```json
{
  "idle_rss_mb_median": {"target": 100, "soft": 150, "hard": 200},
  "cold_start_ms":      {"target": 100, "soft": 300, "hard": 1000},
  "goroutines_idle":    {"target": 15,  "soft": 30,  "hard": 100},
  "binary_size_mb":     {"target": 30,  "soft": 40,  "hard": 60}
}
```

Exit codes: `0` under `soft`, `1` between `soft` and `hard` (warn),
`2` above `hard` (fail). CI wires the hard fail into a pre-merge
check once we land CI. For now it's a local pre-commit prompt.

### 3. Pre-commit hook — `hooks/pre-commit-bench-check`

Only runs when the diff touches `core/`, `distro/`, `plugin-api/`, or
`plugins/`. Skips for pure docs / hack / ui changes.

```bash
touched_go=$(git diff --cached --name-only | grep -E '^(core|distro|plugin-api|plugins)/.*\.go$' || true)
[ -z "$touched_go" ] && exit 0
make bench-check || {
  echo "bench guardrail failed — see hack/bench/results/latest/summary.md"
  echo "if this is an expected regression, run: git commit --no-verify"
  exit 1
}
```

Total pre-commit added latency: ~70 s (5 s warmup + 60 s idle sample
+ 5 s cold start + heap capture). Acceptable for backend changes;
skipped for the frequent UI-only commits.

### 4. Historical trend — `hack/bench/results/history.jsonl`

Every `bench-check` run appends one JSONL row with `{ts, commit, host,
idle_rss_mb, cold_start_ms, binary_mb, goroutines}`. A one-line
`awk` script plots regression:

```
awk -F'"' '/idle_rss_mb/{print $4}' hack/bench/results/history.jsonl
```

No dashboard, no service. Just a file we grep when the number
changes.

### 5. Bench-first for perf work

Anyone touching argon2, sqlite, postingest, or the HTTP mux runs:

1. `./hack/bench/bench.sh --scenario 02 --scenario 07` (baseline).
2. Change.
3. Rerun. Diff `summary.md`.
4. Include the two numbers in the commit body:
   `idle 161.7 MB → 88.4 MB (-73.3 MB)`.

## Immediate next steps

1. Land R1 (argon2 FreeOSMemory) — smallest change, biggest impact.
   Rerun scenario 02; expect ~90 MB idle. Adjust
   `README.md:28` / `docs/index.mdx:21` / `docs/architecture.mdx:88`
   from `~100 MB` to the measured number.
2. Land the guardrail wiring (thresholds.json, bench_check_thresholds,
   Makefile target, pre-commit hook).
3. Rerun scenario 04 with the corrected 900 s timeout to publish the
   real "100 MB PDF ingest → done" wall time. That number lands in
   the headline table.
4. If R1 alone hits < 100 MB idle, defer R2-R5. YAGNI — only cut
   further when a concrete constraint (VPS tier, container OOMKill)
   demands it.
