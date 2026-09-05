.PHONY: help build test vet lint fmt fmt-check tidy check security-check run clean smoke smoke-ingest smoke-mail install-hooks ui ui-dev ui-check ui-e2e ui-clean docs-dev docs-check bench-check release

.DEFAULT_GOAL := help

BIN := $(CURDIR)/dist/suchi
MODULES := . plugin-api hack/emlfixtures hack/bench/tools/sampler hack/bench/tools/gen-pdf hack/bench/tools/report
TEST_FLAGS ?= -timeout 60s
GO_FILES = find . \( -name .git -o -name node_modules -o -name vendor \) -prune -o -type f -name '*.go' -print0
STATICCHECK_VERSION := v0.8.0
GOVULNCHECK_VERSION := v1.7.0
MINT_VERSION := 4.2.817

help:
	@printf '%s\n' \
	  'Available targets:' \
	  '  help            Show this target list.' \
	  '  build           Build the production suchi binary.' \
	  '  test            Run tests in every Go module.' \
	  '  vet             Run go vet in every Go module.' \
	  '  lint            Run the pinned staticcheck across Go modules.' \
	  '  fmt             Format every Go source file.' \
	  '  fmt-check       Fail when a Go source file needs formatting.' \
	  '  tidy            Run go mod tidy in every Go module.' \
	  '  check           Run formatting, vet, tests, lint, and UI checks.' \
	  '  security-check  Run govulncheck and the Bun dependency audit.' \
	  '  run             Build and run a local server on port 8000.' \
	  '  clean           Remove the built binary directory.' \
	  '  smoke           Build and smoke-test server health endpoints.' \
	  '  smoke-ingest    Exercise the local document ingestion path.' \
	  '  smoke-mail      Exercise the mbsync mail deployment path.' \
	  '  ui              Build and copy the committed embedded SPA.' \
	  '  ui-dev          Run the Vite development server.' \
	  '  ui-check        Check, test, build, and compare embedded SPA assets.' \
	  '  ui-e2e          Run the Playwright browser suite.' \
	  '  ui-clean        Remove UI dependencies and generated assets.' \
	  '  docs-dev        Run the Mintlify documentation server.' \
	  '  docs-check      Check documentation for broken links.' \
	  '  install-hooks   Install the tracked Git hooks.' \
	  '  bench-check     Run benchmark scenarios against hard limits.' \
	  '  release         Dispatch the release workflow for VERSION.' 

build:
	@mkdir -p dist
	CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o $(BIN) ./distro/cmd/suchi
	@echo "built $(BIN) ($$(du -h $(BIN) | cut -f1))"

test:
	@for m in $(MODULES); do echo "=== test $$m ==="; ( cd "$$m" && GOWORK=off go test $(TEST_FLAGS) ./... ) || exit 1; done

vet:
	@for m in $(MODULES); do echo "=== vet $$m ==="; ( cd "$$m" && GOWORK=off go vet ./... ) || exit 1; done

lint:
	@tool="$$(command -v staticcheck || printf '%s/bin/staticcheck' "$$(go env GOPATH)")"; \
	  want="$(patsubst v%,%,$(STATICCHECK_VERSION))"; actual=""; \
	  test ! -x "$$tool" || actual="$$($$tool -version 2>/dev/null || true)"; \
	  case "$$actual" in *"($$want)") ;; *) go install honnef.co/go/tools/cmd/staticcheck@$(STATICCHECK_VERSION);; esac; \
	  for m in $(MODULES); do echo "=== lint $$m ==="; \
	    ( cd "$$m" && GOWORK=off "$$tool" ./... ) || exit 1; \
	  done

fmt:
	@$(GO_FILES) | xargs -0 gofmt -w

fmt-check:
	@set -e; drift="$$( $(GO_FILES) | xargs -0 gofmt -l )"; \
	  test -z "$$drift" || { printf '%s\n' "$$drift" 'Run make fmt to format these files.'; exit 1; }

tidy:
	@for m in $(MODULES); do echo "=== tidy $$m ==="; ( cd "$$m" && GOWORK=off go mod tidy ) || exit 1; done

check: fmt-check vet test lint ui-check

# Release-time advisory scan. Kept separate from `check` because it queries
# external vulnerability databases and may install the pinned scanner.
security-check:
	@tool="$$(command -v govulncheck || printf '%s/bin/govulncheck' "$$(go env GOPATH)")"; \
	  actual=""; go_version="$$(go env GOVERSION)"; \
	  test ! -x "$$tool" || actual="$$($$tool -version 2>/dev/null || true)"; \
	  case "$$actual" in *"Go: $$go_version"*"Scanner: govulncheck@$(GOVULNCHECK_VERSION)"*) ;; \
	    *) go install golang.org/x/vuln/cmd/govulncheck@$(GOVULNCHECK_VERSION);; esac; \
	  "$$tool" ./...
	@cd ui && bun audit

# Convenience: build + smoke-test the running server.
smoke: build
	@set -eu; smoke_dir="$$(mktemp -d /tmp/suchi-smoke.XXXXXXXX)"; smoke_pid=''; \
	  trap 'test -z "$$smoke_pid" || { kill "$$smoke_pid" 2>/dev/null || true; wait "$$smoke_pid" 2>/dev/null || true; }; rm -rf "$$smoke_dir"' EXIT; \
	  trap 'exit 1' HUP INT TERM; \
	  port="$${PORT:-8765}"; url="http://127.0.0.1:$$port"; \
	  if curl -s --max-time 1 "$$url/healthz" >/dev/null; then \
	    printf 'smoke: %s is already serving; choose another PORT\n' "$$url" >&2; exit 1; \
	  fi; \
	  PUBLIC_URL="$$url" LISTEN_ADDR="127.0.0.1:$$port" DATA_DIR="$$smoke_dir" \
	    SUCHI_DEV=0 SUCHI_DEMO_MODE=0 OIDC_ISSUER_URL='' INGEST_FS_DIR='' \
	    INGEST_FS_OWNER_EMAIL='' LLM_ENDPOINT_URL='' \
	    "$(BIN)" serve >"$$smoke_dir/server.log" 2>&1 & smoke_pid=$$!; \
	  attempts=0; \
	  until curl -fsS --max-time 1 "$$url/healthz" >/dev/null 2>&1 && \
	        curl -fsS --max-time 1 "$$url/readyz" >/dev/null 2>&1; do \
	    attempts=$$((attempts + 1)); \
	    if ! kill -0 "$$smoke_pid" 2>/dev/null || test "$$attempts" -ge 60; then \
	      cat "$$smoke_dir/server.log" >&2; exit 1; \
	    fi; \
	    sleep 0.1; \
	  done; \
	  kill -0 "$$smoke_pid"; \
	  printf 'smoke: health and readiness passed at %s\n' "$$url"

run: build
	@mkdir -p /tmp/suchi-dev
	PUBLIC_URL=http://127.0.0.1:8000 DATA_DIR=/tmp/suchi-dev $(BIN) serve

clean:
	rm -rf dist

# Manually trigger the release workflow. Requires `gh` and an existing
# origin tag (create with `git tag -s v0.1.0 && git push origin v0.1.0`).
# `make release VERSION=v0.1.0` builds + publishes; `PUBLISH=false` runs
# the artifacts-only smoke path.
release:
	@test -n "$(VERSION)" || (echo "usage: make release VERSION=v0.1.0 [PUBLISH=false]"; exit 1)
	@command -v gh >/dev/null || (echo "gh CLI is required (https://cli.github.com)"; exit 1)
	@git rev-parse --verify "refs/tags/$(VERSION)" >/dev/null 2>&1 || \
	  (echo "tag $(VERSION) not found locally — create it first: git tag -s $(VERSION) && git push origin $(VERSION)"; exit 1)
	gh workflow run release.yml --ref $(VERSION) -f publish=$(or $(PUBLISH),true)
	@echo "dispatched release.yml at $(VERSION) (publish=$(or $(PUBLISH),true))"
	@echo "watch: gh run watch --workflow release.yml"

# Build the Svelte SPA and refresh core/ui/spa/dist (embedded into the
# Go binary). Contributors who don't touch the UI do not need Bun; the
# built dist is committed.
ui:
	@cd ui && bun install --frozen-lockfile && bun run build
	@rm -rf core/ui/spa/dist && mkdir -p core/ui/spa
	@cp -r ui/dist core/ui/spa/dist
	@echo "embedded $$(du -sh core/ui/spa/dist | cut -f1) — commit core/ui/spa/dist"

ui-dev:
	@cd ui && bun run dev

ui-check:
	@cd ui && bun install --frozen-lockfile && bun run check && bun run test && bun run build
	@diff -qr ui/dist core/ui/spa/dist

ui-e2e:
	@cd ui && bun install --frozen-lockfile && bun run e2e

ui-clean:
	rm -rf core/ui/spa/dist ui/dist ui/node_modules

docs-dev:
	@cd docs && BUN_TMPDIR=$${BUN_TMPDIR:-/tmp} bunx mint@$(MINT_VERSION) dev

docs-check:
	@cd docs && BUN_TMPDIR=$${BUN_TMPDIR:-/tmp} bunx mint@$(MINT_VERSION) broken-links

smoke-ingest:
	@./hack/local-ingest-test.sh

smoke-mail:
	@cd deploy/mail-mbsync && ./smoke-test.sh

# Resolve hooks through Git so installation also works in linked worktrees.
install-hooks:
	@set -e; hooks_dir="$$(git rev-parse --git-path hooks)"; mkdir -p "$$hooks_dir"; \
	  for f in hooks/*; do \
	  case "$$f" in hooks/README.md) continue;; esac; \
	  install -m 0755 "$$f" "$$hooks_dir/$$(basename "$$f")"; \
	  echo "installed $$hooks_dir/$$(basename "$$f")"; \
	done

# Perf guardrail: rebuild suchi and re-measure idle RAM, cold start,
# goroutine count, binary size. Fails hard if any metric exceeds the
# `hard` threshold in hack/bench/thresholds.json.
bench-check: build
	@./hack/bench/bench.sh --scenario 01 --scenario 02 --scenario 03 --scenario 07 --thresholds
