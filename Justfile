# Justfile — common commands, `just` (github.com/casey/just) as a Make
# alternative for developer ergonomics. Discoverable via `just` (lists
# recipes) or `just --list-heading '' --list`.
#
# The Makefile stays authoritative for CI + scripts; recipes here mirror
# it and add opinionated shortcuts for the day-to-day loop.

BIN := justfile_directory() / "dist/suchi"
DATA := "/tmp/suchi-dev"

# Default recipe — print the list.
default:
    @just --list

# --- build + test ---

# Compile the suchi binary → ./dist/suchi.
build:
    @mkdir -p dist
    cd distro && CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o {{BIN}} ./cmd/suchi
    @echo "built {{BIN}} ($(du -h {{BIN}} | cut -f1))"

# Run all module tests.
test:
    make test

# gofmt + staticcheck.
lint:
    gofmt -l core hack plugins distro plugin-api
    make lint

# Run gofmt in-place.
fmt:
    make fmt

# `go mod tidy` across every module.
tidy:
    make tidy

# --- run modes ---

# Boot suchi on :8000 pointed at /tmp/suchi-dev. Wipes prior data by
# default — pass `just serve keep` to keep the previous dev DB.
serve keep="":
    #!/usr/bin/env bash
    set -euo pipefail
    if [[ "{{keep}}" != "keep" ]]; then rm -rf {{DATA}}; fi
    mkdir -p {{DATA}}
    PUBLIC_URL=http://127.0.0.1:8000 \
    DATA_DIR={{DATA}} \
    LISTEN_ADDR=:8000 \
    LOG_LEVEL=info \
    {{BIN}} serve

# Wipe the dev DATA_DIR and start fresh. Common when the setup token
# has been used, admin creds forgotten, or you want a clean slate.
reset:
    rm -rf {{DATA}}
    @echo "reset {{DATA}}"

# Show the current dev instance's setup token (if suchi is running).
setup-token:
    grep 'localauth.setup.token_minted' {{DATA}}/suchi.log \
      | grep -oP '"token":"\K[^"]+' | head -1

# Sanity-check a running dev instance.
doctor port="8000":
    #!/usr/bin/env bash
    set -e
    echo "== /healthz =="; curl -sf http://127.0.0.1:{{port}}/healthz && echo
    echo "== /readyz =="; curl -sf http://127.0.0.1:{{port}}/readyz && echo
    echo "== egress surface =="
    grep 'egress.surface' {{DATA}}/suchi.log 2>/dev/null | tail -1 || echo "(no log yet)"
    echo "== disk =="
    du -sh {{DATA}} 2>/dev/null || echo "(no data dir)"

# --- deploy recipe helpers ---

# Run the mail-mbsync deploy recipe's smoke test (Docker; ~90s).
smoke-mail:
    cd deploy/mail-mbsync && ./smoke-test.sh

# Run the local ingest chain against synthetic .eml fixtures.
smoke-ingest:
    ./hack/local-ingest-test.sh

# --- git helpers ---

# One-shot check before pushing: fmt clean, staticcheck clean, tests pass.
check:
    just fmt
    just lint
    just test

# --- housekeeping ---

# Kill any suchi process listening on the given port.
kill port="8000":
    #!/usr/bin/env bash
    pid=$(lsof -t -i :{{port}} 2>/dev/null | head -1) || true
    if [[ -n "$pid" ]]; then kill "$pid" && echo "killed $pid"; else echo "no listener on :{{port}}"; fi

# Nuke build outputs.
clean:
    rm -rf dist
