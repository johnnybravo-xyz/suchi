#!/usr/bin/env bash
#
# Shared functions for the suchi bench harness. Sourced by bench.sh
# and by each scenario subshell. No side effects at source time —
# only function/constant definitions.
#
# Convention: every function that mutates process state exports the
# vars it sets so caller subshells inherit them without extra glue.

set -euo pipefail

# Repo root is two levels up from this file (hack/bench/lib.sh -> repo).
BENCH_LIB_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="${REPO_ROOT:-$(cd "$BENCH_LIB_DIR/../.." && pwd)}"
export REPO_ROOT

# Defaults; callers may override before sourcing.
PORT="${PORT:-8765}"
SUCHI_BIN="${SUCHI_BIN:-$REPO_ROOT/dist/suchi}"
BENCH_DIR="$REPO_ROOT/hack/bench"
TOOLS_DIR="$BENCH_DIR/tools"
export PORT SUCHI_BIN BENCH_DIR TOOLS_DIR

# ---------------------------------------------------------------------------
# bench_setup_dirs — create a timestamped results dir and export RESULTS_DIR.
# Idempotent within a single second: if the dir already exists, reuse it.
# ---------------------------------------------------------------------------
bench_setup_dirs() {
    local ts
    ts="$(date +%Y%m%d-%H%M%S)"
    RESULTS_DIR="${RESULTS_DIR:-$BENCH_DIR/results/$ts}"
    mkdir -p "$RESULTS_DIR"
    export RESULTS_DIR
    echo "results dir: $RESULTS_DIR" >&2
}

# ---------------------------------------------------------------------------
# bench_build_tools — build sampler + gen-pdf + report into tools/*/bin.
# Skips a tool if its binary is newer than its main.go (cheap freshness check).
# ---------------------------------------------------------------------------
bench_build_tools() {
    local tool src bin
    for tool in sampler gen-pdf report; do
        src="$TOOLS_DIR/$tool"
        bin="$src/bin/$tool"
        if [ ! -d "$src" ]; then
            echo "bench_build_tools: missing $src — did the Go tools land yet?" >&2
            return 1
        fi
        if [ -x "$bin" ] && [ -f "$src/main.go" ] && [ "$bin" -nt "$src/main.go" ]; then
            continue
        fi
        mkdir -p "$src/bin"
        # Tools have their own go.mod and are not part of the repo's go.work;
        # GOWORK=off avoids "no such module" from the outer workspace.
        ( cd "$src" && GOWORK=off go build -o "bin/$tool" . )
        echo "built $bin" >&2
    done
    SAMPLER_BIN="$TOOLS_DIR/sampler/bin/sampler"
    GEN_PDF_BIN="$TOOLS_DIR/gen-pdf/bin/gen-pdf"
    REPORT_BIN="$TOOLS_DIR/report/bin/report"
    export SAMPLER_BIN GEN_PDF_BIN REPORT_BIN
}

bench_start_suchi() {
    PUBLIC_URL="http://127.0.0.1:$SUCHI_PORT" \
    LISTEN_ADDR="127.0.0.1:$SUCHI_PORT" \
    DATA_DIR="$DATA_DIR" \
    LOG_LEVEL=warn \
    SUCHI_DEV=0 \
    SUCHI_DEMO_MODE=0 \
    OIDC_ISSUER_URL='' \
    INGEST_FS_DIR='' \
    INGEST_FS_OWNER_EMAIL='' \
    LLM_ENDPOINT_URL='' \
    "$SUCHI_BIN" serve > "$SUCHI_LOG" 2>&1 &
    SUCHI_PID=$!
    export SUCHI_PID
}

# Spawn a clean instance and wait for its health endpoint.
bench_boot_suchi() {
    SUCHI_PORT="${SUCHI_PORT:-$PORT}"
    if curl -sS --max-time 1 -o /dev/null "http://127.0.0.1:$SUCHI_PORT/healthz" 2>/dev/null; then
        echo "bench_boot_suchi: port $SUCHI_PORT already serves HTTP; choose another PORT" >&2
        return 1
    fi
    DATA_DIR="$(mktemp -d -t suchi-bench-XXXXXXXX)"
    case "$DATA_DIR" in
        /tmp/*) : ;;
        *) echo "bench_boot_suchi: refusing DATA_DIR=$DATA_DIR (not under /tmp/)" >&2; return 1 ;;
    esac
    SUCHI_LOG="$DATA_DIR/suchi.log"
    export SUCHI_PORT DATA_DIR SUCHI_LOG
    bench_start_suchi

    for _ in $(seq 1 80); do
        if ! kill -0 "$SUCHI_PID" 2>/dev/null; then break; fi
        if curl -sfS "http://127.0.0.1:$SUCHI_PORT/healthz" >/dev/null 2>&1; then
            echo "suchi up on :$SUCHI_PORT (pid=$SUCHI_PID data=$DATA_DIR)" >&2
            return 0
        fi
        sleep 0.25
    done
    echo "bench_boot_suchi: /healthz never came up. tail of $SUCHI_LOG:" >&2
    tail -50 "$SUCHI_LOG" >&2 || true
    return 1
}

# ---------------------------------------------------------------------------
# bench_bootstrap_admin — scrape the setup token from the log, POST /setup,
# Login for an admin cookie, then mint ADMIN_TOKEN for document calls.
# Requires bench_boot_suchi to have run first. Setup log line is emitted
# at LOG_LEVEL=warn because it's a one-shot bootstrap event.
# ---------------------------------------------------------------------------
bench_bootstrap_admin() {
    ADMIN_EMAIL="${ADMIN_EMAIL:-bench-admin@bench.local}"
    local password="${ADMIN_PASSWORD:-bench-passwd-1}"
    export ADMIN_EMAIL

    local token=""
    for _ in $(seq 1 40); do
        token="$(grep 'localauth.setup.token_minted' "$SUCHI_LOG" 2>/dev/null \
            | grep -oP '"token":"\K[^"]+' | head -1 || true)"
        if [ -n "$token" ]; then break; fi
        sleep 0.25
    done
    if [ -z "$token" ]; then
        echo "bench_bootstrap_admin: setup token never appeared in $SUCHI_LOG" >&2
        tail -30 "$SUCHI_LOG" >&2 || true
        return 1
    fi

    curl -sfS -X POST "http://127.0.0.1:$SUCHI_PORT/setup" \
        -H 'Content-Type: application/json' \
        -d "{\"token\":\"$token\",\"email\":\"$ADMIN_EMAIL\",\"password\":\"$password\"}" \
        >/dev/null

    ADMIN_COOKIES="$DATA_DIR/admin.cookies"
    curl -sfS -X POST "http://127.0.0.1:$SUCHI_PORT/api/login" \
        --cookie-jar "$ADMIN_COOKIES" \
        -H 'Content-Type: application/json' \
        -d "{\"email\":\"$ADMIN_EMAIL\",\"password\":\"$password\"}" >/dev/null
    export ADMIN_COOKIES

    ADMIN_TOKEN="$(curl -sfS -X POST "http://127.0.0.1:$SUCHI_PORT/api/token/" \
        -H 'Accept: application/json' -H 'Content-Type: application/json' \
        -d "{\"email\":\"$ADMIN_EMAIL\",\"password\":\"$password\"}" \
        | grep -oP '"token":"\K[^"]+' | head -1)"
    if [ -z "${ADMIN_TOKEN:-}" ]; then
        echo "bench_bootstrap_admin: /api/token/ returned no token" >&2
        return 1
    fi
    export ADMIN_TOKEN
    echo "admin bootstrapped: $ADMIN_EMAIL" >&2
}

# ---------------------------------------------------------------------------
# bench_mint_member <email> <password> — create a capability-empty user and
# log them in. Echoes the API token on stdout for the caller to capture.
# Requires ADMIN_COOKIES. Non-fatal 409 on duplicate creation is tolerated.
# ---------------------------------------------------------------------------
bench_mint_member() {
    local email="$1" password="$2" status
    status="$(curl -sS -o /dev/null -w '%{http_code}' -X POST "http://127.0.0.1:$SUCHI_PORT/api/admin/users" \
        --cookie "$ADMIN_COOKIES" \
        -H 'Sec-Fetch-Site: same-origin' -H 'Content-Type: application/json' \
        -d "{\"email\":\"$email\",\"password\":\"$password\",\"capabilities\":[]}")"
    if [ "$status" != 201 ] && [ "$status" != 409 ]; then
        echo "bench_mint_member: account creation returned HTTP $status" >&2
        return 1
    fi
    curl -sfS -X POST "http://127.0.0.1:$SUCHI_PORT/api/token/" \
        -H 'Accept: application/json' -H 'Content-Type: application/json' \
        -d "{\"email\":\"$email\",\"password\":\"$password\"}" \
        | grep -oP '"token":"\K[^"]+' | head -1
}

# ---------------------------------------------------------------------------
# bench_teardown — stop suchi, drop the datadir. Idempotent; safe to call
# from EXIT traps even if boot failed. Hard-refuses to rm outside /tmp/.
# ---------------------------------------------------------------------------
bench_teardown() {
    if [ -n "${SAMPLER_PID:-}" ]; then
        kill -TERM "$SAMPLER_PID" 2>/dev/null || true
        wait "$SAMPLER_PID" 2>/dev/null || true
        SAMPLER_PID=""
    fi
    if [ -n "${SUCHI_PID:-}" ]; then
        # Verify the pid still belongs to us before signalling.
        if kill -0 "$SUCHI_PID" 2>/dev/null; then
            kill -TERM "$SUCHI_PID" 2>/dev/null || true
            wait "$SUCHI_PID" 2>/dev/null || true
        fi
        SUCHI_PID=""
    fi
	if [ -n "${DATA_DIR:-}" ] && [ -d "$DATA_DIR" ]; then
		if [ "${BENCH_KEEP:-0}" = "1" ]; then
			echo "kept DATA_DIR=$DATA_DIR" >&2
			return 0
		fi
		case "$DATA_DIR" in
            /tmp/*) rm -rf "$DATA_DIR" ;;
            *) echo "bench_teardown: refusing rm -rf $DATA_DIR (not under /tmp/)" >&2 ;;
        esac
        DATA_DIR=""
    fi
}

# ---------------------------------------------------------------------------
# bench_wait_jobs_drain <timeout_s> — poll the jobs table until no rows are
# in 'pending' or 'running'. Return 0 on drain, 1 on timeout.
# ---------------------------------------------------------------------------
bench_wait_jobs_drain() {
    local timeout="${1:-300}"
    local db="$DATA_DIR/suchi.db"
    local n
    for _ in $(seq 1 "$timeout"); do
		n="$(sqlite3 "$db" "SELECT COUNT(*) FROM jobs WHERE state = 'running' OR (state = 'pending' AND next_run_at <= unixepoch())" 2>/dev/null || echo 999)"
        if [ "$n" = "0" ]; then return 0; fi
        sleep 1
	done
	echo "bench_wait_jobs_drain: timeout after ${timeout}s, still $n job(s) in flight" >&2
	sqlite3 -header -column "$db" \
		"SELECT id, kind, state, attempts, last_error FROM jobs WHERE state = 'running' OR (state = 'pending' AND next_run_at <= unixepoch())" >&2 || true
	tail -50 "$SUCHI_LOG" >&2 || true
	return 1
}

# ---------------------------------------------------------------------------
# bench_wait_job_done <doc_id> <timeout_s> — poll the postingest job for a
# specific doc. Return 0 on 'done', 1 on 'dead' or timeout.
# ---------------------------------------------------------------------------
bench_wait_job_done() {
    local doc_id="$1"
    local timeout="${2:-300}"
    local db="$DATA_DIR/suchi.db"
    local state
    for _ in $(seq 1 "$timeout"); do
        state="$(sqlite3 "$db" "SELECT state FROM jobs WHERE doc_id=$doc_id AND kind='post-ingest' ORDER BY id DESC LIMIT 1" 2>/dev/null || echo "")"
        case "$state" in
            done) return 0 ;;
            dead) echo "bench_wait_job_done: doc=$doc_id job died" >&2; return 1 ;;
        esac
        sleep 1
    done
    echo "bench_wait_job_done: timeout after ${timeout}s for doc=$doc_id (last state=$state)" >&2
    return 1
}

# ---------------------------------------------------------------------------
# bench_start_sampler <out.jsonl> — run the sampler against $SUCHI_PID in the
# background at 100ms cadence. Exports SAMPLER_PID.
# ---------------------------------------------------------------------------
bench_start_sampler() {
    local out="$1"
    if [ -z "${SUCHI_PID:-}" ]; then
        echo "bench_start_sampler: SUCHI_PID unset" >&2; return 1
    fi
    "$SAMPLER_BIN" -pid "$SUCHI_PID" -interval 100ms -out "$out" &
    SAMPLER_PID=$!
    export SAMPLER_PID
    # Small settling window so the sampler has opened its file.
    sleep 0.2
}

# ---------------------------------------------------------------------------
# bench_stop_sampler — SIGTERM the sampler and wait. Idempotent.
# ---------------------------------------------------------------------------
bench_stop_sampler() {
    if [ -n "${SAMPLER_PID:-}" ]; then
        kill -TERM "$SAMPLER_PID" 2>/dev/null || true
        wait "$SAMPLER_PID" 2>/dev/null || true
        SAMPLER_PID=""
    fi
}

# ---------------------------------------------------------------------------
# bench_check_thresholds [results_dir] — guardrail gate. Reads the four
# tracked metrics from the given results dir (or $RESULTS_DIR, or the most
# recent results/*/ if neither is set), compares each against
# hack/bench/thresholds.json, prints a one-line-per-metric table, and
# returns 0 if all metrics are at-or-below `hard` (warnings are non-fatal),
# 2 if any metric exceeds `hard`. Missing scenario data is SKIPped.
# ---------------------------------------------------------------------------
bench_check_thresholds() {
    local results_dir="${1:-${RESULTS_DIR:-}}"
    if [ -z "$results_dir" ]; then
        # Fall back to the most recent results dir.
        results_dir="$(find "$BENCH_LIB_DIR/results" -maxdepth 1 -mindepth 1 -type d 2>/dev/null | sort | tail -1)"
    fi
    if [ -z "$results_dir" ] || [ ! -d "$results_dir" ]; then
        echo "bench_check_thresholds: no results dir found" >&2
        return 2
    fi
    local thresholds="$BENCH_LIB_DIR/thresholds.json"
    if [ ! -f "$thresholds" ]; then
        echo "bench_check_thresholds: missing $thresholds" >&2
        return 2
    fi

    local green="\e[32m" yellow="\e[33m" red="\e[31m" reset="\e[0m"

    # Metric extractors. Each echoes the numeric value or nothing when the
    # source file is absent/malformed.

    # idle_rss_mb_median: median of rss_kb samples in 02-idle-ram.jsonl, in MB.
    local idle_file="$results_dir/02-idle-ram.jsonl"
    local idle_val=""
    if [ -f "$idle_file" ] && [ -s "$idle_file" ]; then
        idle_val="$(jq -r '.rss_kb' "$idle_file" 2>/dev/null | sort -n | awk '
            { a[NR] = $1 }
            END {
                if (NR == 0) exit 0
                if (NR % 2 == 1) m = a[(NR + 1) / 2]
                else m = (a[NR / 2] + a[NR / 2 + 1]) / 2
                printf "%.1f", m / 1024
            }')"
    fi

    # cold_start_ms: .value in 03-cold-start.summary.json.
    local cold_file="$results_dir/03-cold-start.summary.json"
    local cold_val=""
    if [ -f "$cold_file" ]; then
        cold_val="$(jq -r '.value // empty' "$cold_file" 2>/dev/null)"
    fi

    # goroutines_idle: prefer the profile header "goroutine profile: total N"
    # in 07-goroutine-idle.txt (the summary.json field is buggy — counts
    # frame lines rather than goroutines). Fall back to the summary field
    # only if the profile file is absent.
    local goro_file="$results_dir/07-goroutine-idle.txt"
    local goro_summary="$results_dir/07-mem-profile.summary.json"
    local goro_val=""
    if [ -f "$goro_file" ]; then
        goro_val="$(head -1 "$goro_file" 2>/dev/null | grep -oE 'total [0-9]+' | awk '{print $2}')"
    elif [ -f "$goro_summary" ]; then
        goro_val="$(jq -r '.goroutines_idle // empty' "$goro_summary" 2>/dev/null)"
    fi

    # binary_size_mb: bytes of the "dist/suchi" artifact in
    # 01-binary-size.summary.json → MB, 1 decimal.
    local bin_file="$results_dir/01-binary-size.summary.json"
    local bin_val=""
    if [ -f "$bin_file" ]; then
        bin_val="$(jq -r '(.artifacts[] | select(.name == "dist/suchi") | .bytes) // empty' "$bin_file" 2>/dev/null \
            | awk 'NR==1 && $1 != "" { printf "%.1f", $1 / 1024 / 1024 }')"
    fi

    local overall_rc=0

    _bench_check_one() {
        # $1=metric key, $2=value (may be empty), $3=display width for value
        local key="$1" val="$2" width="$3"
        local target soft hard
        target="$(jq -r --arg k "$key" '.[$k].target' "$thresholds")"
        soft="$(jq -r --arg k "$key" '.[$k].soft' "$thresholds")"
        hard="$(jq -r --arg k "$key" '.[$k].hard' "$thresholds")"
        local band_fmt
        band_fmt="$(printf '[target=%s soft=%s hard=%s]' "$target" "$soft" "$hard")"

        if [ -z "$val" ]; then
            printf "  %-22s %-${width}s %-32s ${yellow}SKIP${reset}\n" \
                "$key:" "-" "$band_fmt"
            return 0
        fi
        # Numeric comparison via awk (handles floats).
        local status_glyph status_word rc_delta=0
        if awk -v v="$val" -v h="$hard" 'BEGIN { exit !(v+0 > h+0) }'; then
            status_glyph="${red}✗${reset}"
            status_word="${red}FAIL${reset}"
            rc_delta=2
        elif awk -v v="$val" -v s="$soft" 'BEGIN { exit !(v+0 > s+0) }'; then
            status_glyph="${yellow}⚠${reset}"
            status_word="${yellow}WARN${reset}"
        else
            status_glyph="${green}✓${reset}"
            status_word="${green}OK${reset}"
        fi
        printf "  %b %-22s %-${width}s %-32s %b\n" \
            "$status_glyph" "$key:" "$val" "$band_fmt" "$status_word"
        if [ "$rc_delta" -gt 0 ]; then
            overall_rc=2
        fi
    }

    echo
    echo "===== guardrail thresholds ====="
    echo "  results dir: $results_dir"
    _bench_check_one "idle_rss_mb_median" "$idle_val" 8
    _bench_check_one "cold_start_ms"      "$cold_val" 8
    _bench_check_one "goroutines_idle"    "$goro_val" 8
    _bench_check_one "binary_size_mb"     "$bin_val"  8

    unset -f _bench_check_one
    return "$overall_rc"
}
