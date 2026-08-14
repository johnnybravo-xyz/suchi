#!/usr/bin/env bash
#
# suchi bench driver. Sets up a fresh results dir, builds the Go tool
# binaries, iterates through scenarios (each in its own subshell so state
# can't leak between them), then runs the reporter over the collected
# artefacts.
#
# Usage:
#   ./bench.sh                       # run all scenarios in order
#   ./bench.sh --scenario 04         # run only 04-single-100mb
#   ./bench.sh --keep                # skip teardown; leak DATA_DIR for post-mortem
#   USERS=20 DOCS_PER_USER=50 ./bench.sh --scenario 06
#
# Notes:
#   --no-dev is accepted for symmetry with older workflows but is a no-op:
#   the harness never relies on dev mode. Setup tokens are scraped from the
#   log at LOG_LEVEL=warn, which emits the bootstrap event unconditionally.

set -euo pipefail

BENCH_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck source=./lib.sh
source "$BENCH_DIR/lib.sh"

SCENARIO_FILTERS=()
KEEP=0
CHECK_THRESHOLDS=0

print_help() {
    cat <<'EOF'
usage: bench.sh [--scenario NN]... [--keep] [--no-dev] [--thresholds]

  --scenario NN   Run only the scenario whose filename starts with NN
                  (e.g. --scenario 04 for 04-single-100mb.sh). Repeatable:
                  --scenario 02 --scenario 03 --scenario 07 runs all three.
  --keep          Do not tear down after each scenario; useful for
                  poking at the DATA_DIR of a failed run.
  --no-dev        Accepted for backward compat; no effect. The harness
                  scrapes setup tokens from the log at LOG_LEVEL=warn
                  and never needs dev mode.
  --thresholds    After scenarios finish, compare the guardrail metrics
                  (idle RSS, cold start, goroutines, binary size) against
                  hack/bench/thresholds.json. Exit 2 on any hard-fail.

Environment:
  USERS, DOCS_PER_USER   Read by scenario 06 (defaults 10, 20).
  PORT                   Override the listen port (default 8765).
EOF
}

while [ $# -gt 0 ]; do
    case "$1" in
        --scenario) SCENARIO_FILTERS+=("$2"); shift 2 ;;
        --scenario=*) SCENARIO_FILTERS+=("${1#*=}"); shift ;;
        --keep) KEEP=1; shift ;;
        --no-dev) shift ;;
        --thresholds) CHECK_THRESHOLDS=1; shift ;;
        -h|--help) print_help; exit 0 ;;
        *) echo "bench.sh: unknown flag: $1" >&2; print_help >&2; exit 2 ;;
    esac
done

export BENCH_KEEP="$KEEP"

# Fatal exits from bench.sh should still surface the results dir path.
on_exit() {
    local rc=$?
    if [ -n "${RESULTS_DIR:-}" ]; then
        echo "results dir: $RESULTS_DIR" >&2
    fi
    exit "$rc"
}
trap on_exit EXIT INT TERM

bench_setup_dirs

# Make sure dist/suchi exists — the harness times cold-start etc., but we
# don't time the build itself.
if [ ! -x "$SUCHI_BIN" ]; then
    echo "bench.sh: building suchi (missing $SUCHI_BIN) ..." >&2
    ( cd "$REPO_ROOT" && make build )
fi

bench_build_tools

# Pick which scenarios to run.
scenarios=()
if [ "${#SCENARIO_FILTERS[@]}" -gt 0 ]; then
    # Accumulate matches for each --scenario NN, dedupe, then sort so
    # scenarios still run in numeric order regardless of flag order.
    tmp=()
    for filter in "${SCENARIO_FILTERS[@]}"; do
        matched=0
        while IFS= read -r -d '' f; do
            tmp+=("$f")
            matched=1
        done < <(find "$BENCH_DIR/scenarios" -maxdepth 1 -type f -name "${filter}-*.sh" -print0 | sort -z)
        if [ "$matched" -eq 0 ]; then
            echo "bench.sh: no scenarios matched --scenario $filter" >&2
            exit 2
        fi
    done
    # Dedupe + sort.
    while IFS= read -r f; do
        scenarios+=("$f")
    done < <(printf '%s\n' "${tmp[@]}" | sort -u)
else
    while IFS= read -r -d '' f; do
        scenarios+=("$f")
    done < <(find "$BENCH_DIR/scenarios" -maxdepth 1 -type f -name '*.sh' -print0 | sort -z)
fi

for scenario in "${scenarios[@]}"; do
    name="$(basename "$scenario" .sh)"
    echo
    echo "===== scenario: $name ====="
    # Each scenario runs in an isolated subshell so its boot/teardown,
    # trap, and env vars can't bleed into siblings.
    (
        set -euo pipefail
        # shellcheck source=./lib.sh
        source "$BENCH_DIR/lib.sh"
        # Re-export the outer results dir explicitly — mktemp'd DATA_DIRs
        # are scenario-local, but the results dir is shared.
        export RESULTS_DIR
        # shellcheck source=/dev/null
        source "$scenario"
    ) || {
        echo "===== scenario $name FAILED (continuing) ====="
    }
done

echo
echo "===== report ====="
"$REPORT_BIN" -dir "$RESULTS_DIR" -out "$RESULTS_DIR/summary.md" || {
    echo "reporter failed" >&2
}
echo "summary: $RESULTS_DIR/summary.md"
if [ -f "$RESULTS_DIR/summary.md" ]; then
    cat "$RESULTS_DIR/summary.md"
fi

if [ "$KEEP" = "1" ]; then
    echo
    echo "--keep set: any scenario DATA_DIRs that opted out of teardown are on disk under /tmp/suchi-bench-*"
fi

if [ "$CHECK_THRESHOLDS" = "1" ]; then
    # Temporarily disable -e so we can capture the guardrail's exit code
    # without tripping the on_exit trap before the summary prints.
    set +e
    bench_check_thresholds "$RESULTS_DIR"
    THRESHOLD_RC=$?
    set -e
    exit "$THRESHOLD_RC"
fi
