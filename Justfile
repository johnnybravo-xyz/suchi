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

# Preserves existing DATA_DIR across reboots. To start clean, `just fresh`.
# The binary itself only mints a setup token when users table is empty, so
# existing installs boot straight into normal service.
# Boot suchi on :8000 against DATA_DIR (default /tmp/suchi-dev).
serve:
    #!/usr/bin/env bash
    set -euo pipefail
    mkdir -p {{DATA}}
    PUBLIC_URL=http://127.0.0.1:8000 \
    DATA_DIR={{DATA}} \
    LISTEN_ADDR=:8000 \
    LOG_LEVEL=info \
    {{BIN}} serve

# Setup token used, admin creds forgotten, or you want a clean slate.
# Wipe DATA_DIR and boot suchi against a fresh install.
fresh:
    rm -rf {{DATA}}
    just serve

# Wipe the dev DATA_DIR (without booting).
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

# --- svelte SPA (ui/) ---

# Build the Svelte UI and refresh the embedded copy at core/ui/spa/dist.
# Prefers bun (fast, single binary) and falls back to npm — same output
# either way, so the committed dist is toolchain-agnostic. Contributors
# who don't touch the UI never need either.
ui-build:
    #!/usr/bin/env bash
    set -euo pipefail
    cd ui
    if command -v bun >/dev/null 2>&1; then
        bun install --frozen-lockfile
        bun run build
    else
        npm ci
        npm run build
    fi
    cd ..
    rm -rf core/ui/spa/dist
    mkdir -p core/ui/spa
    cp -r ui/dist core/ui/spa/dist
    echo "embedded $(du -sh core/ui/spa/dist | cut -f1) — commit core/ui/spa/dist"

# Vite dev server on :5173, proxying /api /preview /download /login to
# the Go binary on :8000. Run `just serve` in another terminal first.
ui-dev:
    #!/usr/bin/env bash
    set -euo pipefail
    cd ui
    if command -v bun >/dev/null 2>&1; then
        bun run dev
    else
        npm run dev
    fi

# --- docs ---

# Serve the Mintlify docs locally on :3000 with live-reload. Uses bun
# by default; falls back to npx if bun isn't installed. See
# docs/docs.json for the site structure.
docs-serve:
    #!/usr/bin/env bash
    set -euo pipefail
    cd docs
    if command -v bun >/dev/null 2>&1; then
        bunx mint dev
    elif command -v npx >/dev/null 2>&1; then
        npx mint@latest dev
    else
        echo "install bun or node — bunx or npx must be on PATH"
        exit 1
    fi

# Check the docs for broken cross-links before pushing.
docs-lint:
    #!/usr/bin/env bash
    set -euo pipefail
    cd docs
    if command -v bun >/dev/null 2>&1; then
        bunx mint broken-links
    else
        npx mint@latest broken-links
    fi

# --- deploy recipe helpers ---

# Run the mail-mbsync deploy recipe's smoke test (Docker; ~90s).
smoke-mail:
    cd deploy/mail-mbsync && ./smoke-test.sh

# Run the local ingest chain against synthetic .eml fixtures.
smoke-ingest:
    ./hack/local-ingest-test.sh

# Boot the transcript recorder against `target` (a live upstream URL).
# Point a client at http://127.0.0.1:8443/ and drive it — fixtures
# land in testdata/transcripts/. See hack/transcript/README.md.
transcript target listen=":8443":
    cd hack/transcript && go run . \
        --listen {{listen}} --target {{target}} \
        --out ../../testdata/transcripts

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
