.PHONY: help build test vet lint fmt fmt-check license-check tidy check security-check run clean smoke smoke-ingest smoke-mail install-hooks ui ui-dev ui-check ui-verify ui-tracked-check ui-e2e ui-clean docs-dev docs-metadata-check docs-check bench-check release

.DEFAULT_GOAL := help

BIN := $(CURDIR)/dist/suchi
MODULES := . plugin-api hack/emlfixtures hack/bench/tools/sampler hack/bench/tools/gen-pdf hack/bench/tools/report
TEST_FLAGS ?= -timeout 180s
GO_FILES = find . \( -name .git -o -name node_modules -o -name vendor \) -prune -o -type f -name '*.go' -print0
UI_SRC_FILES = find ui/src -type f \( -name '*.js' -o -name '*.svelte' -o -name '*.css' \) -print0
STATICCHECK_VERSION := v0.8.0
GOVULNCHECK_VERSION := v1.7.0
GITLEAKS_VERSION := v8.30.1
MINT_VERSION := 4.2.874
GITHUB_REMOTE ?= gh
GITHUB_REPO ?= johnnybravo-xyz/suchi

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
	  '  security-check  Run vulnerability, dependency, and repository secret scans.' \
	  '  run             Build and run a local server on port 8000.' \
	  '  clean           Remove the built binary directory.' \
	  '  smoke           Build and smoke-test server health endpoints.' \
	  '  smoke-ingest    Exercise the local document ingestion path.' \
	  '  smoke-mail      Exercise the mbsync mail deployment path.' \
	  '  ui              Build the generated embedded SPA.' \
	  '  ui-dev          Run the Vite development server.' \
	  '  ui-check        Check, test, build, and verify generated SPA assets.' \
	  '  ui-verify       Verify that generated SPA assets match current sources.' \
	  '  ui-e2e          Run the Playwright browser suite.' \
	  '  ui-clean        Remove UI dependencies and generated assets.' \
	  '  docs-dev        Run the Mintlify documentation server.' \
	  '  docs-check      Check documentation for broken links.' \
	  '  docs-metadata-check Check documented versions and image channels.' \
	  '  license-check   Verify every source file declares its licence.' \
	  '  install-hooks   Install the tracked Git hooks.' \
	  '  bench-check     Run benchmark scenarios against hard limits.' \
	  '  release         Dispatch the release workflow for VERSION.' 

build: ui
	@mkdir -p dist
	CGO_ENABLED=0 go build -tags=embedded_ui -trimpath -ldflags="-s -w" -o $(BIN) ./distro/cmd/suchi
	@echo "built $(BIN) ($$(du -h $(BIN) | cut -f1))"

test: ui-verify
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

# Every source file declares its licence. plugin-api/ is Apache-2.0 so third-party
# plugins need not be AGPL; everything else is AGPL-3.0-or-later. Headers carry no
# copyright line — the holder is named in NOTICE and the README License section.
license-check:
	@set -e; \
	  missing="$$( $(GO_FILES) | xargs -0 grep -L 'SPDX-License-Identifier' || true )"; \
	  test -z "$$missing" || { printf '%s\n' "$$missing" '' 'Add: // SPDX-License-Identifier: AGPL-3.0-or-later' >&2; exit 1; }; \
	  missing="$$( $(UI_SRC_FILES) | xargs -0 grep -L 'SPDX-License-Identifier' || true )"; \
	  test -z "$$missing" || { printf '%s\n' "$$missing" '' 'Add an SPDX-License-Identifier header.' >&2; exit 1; }; \
	  wrong="$$( grep -rl 'SPDX-License-Identifier: AGPL' --include='*.go' plugin-api || true )"; \
	  test -z "$$wrong" || { printf '%s\n' "$$wrong" '' 'plugin-api/ must be Apache-2.0, or third-party plugins inherit AGPL.' >&2; exit 1; }; \
	  wrong="$$( grep -rl 'SPDX-License-Identifier: Apache' --include='*.go' core distro plugins hack || true )"; \
	  test -z "$$wrong" || { printf '%s\n' "$$wrong" '' 'Only plugin-api/ is Apache-2.0; the rest is AGPL-3.0-or-later.' >&2; exit 1; }; \
	  printf 'license-check: every source file declares a licence\n'

check: fmt-check license-check ui-check vet test lint

# Release-time advisory and secret scans. Kept separate from `check` because they
# query external databases and may install pinned scanners.
security-check:
	@tool="$$(command -v govulncheck || printf '%s/bin/govulncheck' "$$(go env GOPATH)")"; \
	  actual=""; go_version="$$(go env GOVERSION)"; \
	  test ! -x "$$tool" || actual="$$($$tool -version 2>/dev/null || true)"; \
	  case "$$actual" in *"Go: $$go_version"*"Scanner: govulncheck@$(GOVULNCHECK_VERSION)"*) ;; \
	    *) go install golang.org/x/vuln/cmd/govulncheck@$(GOVULNCHECK_VERSION);; esac; \
	  "$$tool" ./...
	@cd ui && bun audit
	@tool="$$(command -v gitleaks || printf '%s/bin/gitleaks' "$$(go env GOPATH)")"; \
	  want="$(patsubst v%,%,$(GITLEAKS_VERSION))"; actual=""; \
	  test ! -x "$$tool" || actual="$$($$tool version 2>/dev/null || true)"; \
	  test "$$actual" = "$$want" || go install github.com/zricethezav/gitleaks/v8@$(GITLEAKS_VERSION); \
	  "$$tool" git --redact --no-banner --no-color .

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
	  app_status="$$(curl -sS --max-time 2 -o /dev/null -w '%{http_code}' "$$url/app/")"; \
	  test "$$app_status" = 302 || { printf 'smoke: /app/ status %s, want 302 before setup\n' "$$app_status" >&2; exit 1; }; \
	  curl -fsS --max-time 2 "$$url/app/manifest.webmanifest" >/dev/null; \
	  curl -fsS --max-time 2 "$$url/app/third-party-notices.txt" >/dev/null; \
	  set -- core/ui/spa/dist/assets/index-*.js; \
	  test "$$#" -eq 1 && test -f "$$1" || { echo 'smoke: expected one hashed SPA entry asset' >&2; exit 1; }; \
	  asset="$${1#core/ui/spa/dist/}"; \
	  curl -fsS --max-time 2 "$$url/app/$$asset" >/dev/null; \
	  printf 'smoke: health, readiness, and embedded SPA passed at %s\n' "$$url"

run: build
	@mkdir -p /tmp/suchi-dev
	PUBLIC_URL=http://127.0.0.1:8000 DATA_DIR=/tmp/suchi-dev $(BIN) serve

clean:
	rm -rf dist

# Manually trigger the release workflow. Requires `gh` and an existing signed
# annotated tag on the GitHub remote (create with
# `git tag -s v0.1.0 && git push gh v0.1.0`).
# `make release VERSION=v0.1.0` builds + publishes; `PUBLISH=false` runs
# the artifacts-only smoke path. Pipeline proposal policy is committed in
# distro/cmd/suchi/pipeline_proposals.go before the release tag is created.
release:
	@test -n "$(VERSION)" || (echo "usage: make release VERSION=v0.1.0 [PUBLISH=false]"; exit 1)
	@command -v gh >/dev/null || (echo "gh CLI is required (https://cli.github.com)"; exit 1)
	@./hack/verify-release-tag.sh "$(VERSION)"
	@set -eu; \
	  local_tag="$$(git rev-parse --verify "refs/tags/$(VERSION)")"; \
	  local_commit="$$(git rev-parse --verify "refs/tags/$(VERSION)^{}")"; \
	  remote_refs="$$(git ls-remote "$(GITHUB_REMOTE)" "refs/tags/$(VERSION)" "refs/tags/$(VERSION)^{}")" || { \
	    echo "could not read tag $(VERSION) from GitHub remote $(GITHUB_REMOTE)"; exit 1; \
	  }; \
	  remote_tag="$$(printf '%s\n' "$$remote_refs" | awk -v ref="refs/tags/$(VERSION)" '$$2 == ref { print $$1; exit }')"; \
	  remote_commit="$$(printf '%s\n' "$$remote_refs" | awk -v ref="refs/tags/$(VERSION)^{}" '$$2 == ref { print $$1; exit }')"; \
	  test -n "$$remote_commit" || { \
	    echo "signed annotated tag $(VERSION) is not on GitHub — run: git push $(GITHUB_REMOTE) $(VERSION)"; exit 1; \
	  }; \
	  test "$$remote_tag" = "$$local_tag" || { \
	    echo "GitHub tag object $(VERSION) is $$remote_tag, local tag object is $$local_tag"; exit 1; \
	  }; \
	  test "$$remote_commit" = "$$local_commit" || { \
	    echo "GitHub tag $(VERSION) resolves to $$remote_commit, local tag resolves to $$local_commit"; exit 1; \
	  }
	gh workflow run release.yml --repo "$(GITHUB_REPO)" --ref "$(VERSION)" -f "publish=$(or $(PUBLISH),true)"
	@echo "dispatched release.yml at $(VERSION) (publish=$(or $(PUBLISH),true))"
	@echo "watch: gh run watch --repo $(GITHUB_REPO)"

# Build the Svelte SPA into an ignored tree consumed by production Go builds.
# The manifest binds generated bytes to the frontend sources that produced them.
ui:
	@cd ui && bun install --frozen-lockfile && bun run build
	@rm -rf core/ui/spa/dist && mkdir -p core/ui/spa/dist
	@cp -R ui/dist/. core/ui/spa/dist/
	@go run ./hack/ui-assets write ui core/ui/spa/dist
	@go run ./hack/ui-assets verify ui core/ui/spa/dist
	@echo "generated $$(du -sh core/ui/spa/dist | cut -f1) in ignored core/ui/spa/dist"

ui-dev:
	@cd ui && bun run dev

ui-verify:
	@go run ./hack/ui-assets verify ui core/ui/spa/dist || { \
	  echo 'Run `make ui` to rebuild the frontend bundle.' >&2; exit 1; }

ui-tracked-check:
	@set -eu; tracked="$$(git ls-files -- core/ui/spa/dist)"; \
	  test -z "$$tracked" || { printf '%s\n' "$$tracked" '' \
	    'Generated SPA assets must not be tracked. Remove them from Git.' >&2; exit 1; }

ui-check: ui-tracked-check
	@cd ui && bun install --frozen-lockfile && bun run check && bun run test && bun run build
	@rm -rf core/ui/spa/dist && mkdir -p core/ui/spa/dist
	@cp -R ui/dist/. core/ui/spa/dist/
	@go run ./hack/ui-assets write ui core/ui/spa/dist
	@go run ./hack/ui-assets verify ui core/ui/spa/dist

ui-e2e:
	@cd ui && bun install --frozen-lockfile && bun run e2e

ui-clean:
	rm -rf core/ui/spa/dist ui/dist ui/node_modules

docs-dev:
	@cd docs && BUN_TMPDIR=$${BUN_TMPDIR:-/tmp} bunx mint@$(MINT_VERSION) dev

docs-metadata-check:
	@python3 hack/check-release-docs.py

docs-check: docs-metadata-check
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
