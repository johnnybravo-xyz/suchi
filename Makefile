.PHONY: build test vet lint fmt fmt-check tidy check run clean smoke smoke-ingest smoke-mail install-hooks ui ui-dev ui-check ui-clean docs-dev docs-check bench-check release

BIN := $(PWD)/dist/suchi
MODULES := . plugin-api hack/emlfixtures hack/transcript
STATICCHECK_VERSION := v0.7.0

build:
	@mkdir -p dist
	CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o $(BIN) ./distro/cmd/suchi
	@echo "built $(BIN) ($$(du -h $(BIN) | cut -f1))"

test:
	@for m in $(MODULES); do echo "=== test $$m ==="; ( cd $$m && go test -count=1 -timeout 60s ./... ) || exit 1; done

vet:
	@for m in $(MODULES); do echo "=== vet $$m ==="; ( cd $$m && go vet ./... ) || exit 1; done

lint:
	@tool="$$(command -v staticcheck || printf '%s/bin/staticcheck' "$$(go env GOPATH)")"; \
	  test -x "$$tool" || go install honnef.co/go/tools/cmd/staticcheck@$(STATICCHECK_VERSION); \
	  for m in $(MODULES); do echo "=== lint $$m ==="; \
	    ( cd "$$m" && "$$tool" ./... ) || exit 1; \
	  done

fmt:
	gofmt -w $$(find . -type f -name '*.go' -not -path './*/vendor/*')

fmt-check:
	@test -z "$$(gofmt -l $$(find . -type f -name '*.go' -not -path './*/vendor/*'))" || \
	  (gofmt -l $$(find . -type f -name '*.go' -not -path './*/vendor/*'); exit 1)

tidy:
	@for m in $(MODULES); do echo "=== tidy $$m ==="; ( cd $$m && go mod tidy ) || exit 1; done

check: fmt-check vet test lint ui-check

# Convenience: build + smoke-test the running server.
smoke: build
	@rm -rf /tmp/suchi-smoke && mkdir -p /tmp/suchi-smoke
	@PUBLIC_URL=http://127.0.0.1:8765 LISTEN_ADDR=:8765 DATA_DIR=/tmp/suchi-smoke $(BIN) serve & \
	  echo "pid=$$!"; sleep 1; \
	  curl -sf http://127.0.0.1:8765/healthz || (kill $$! ; exit 1); \
	  curl -sf http://127.0.0.1:8765/readyz || (kill $$! ; exit 1); \
	  kill $$!

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
	@cd ui && bun install --frozen-lockfile && bun run check && bun test && bun run build
	@diff -qr ui/dist core/ui/spa/dist

ui-clean:
	rm -rf core/ui/spa/dist ui/dist ui/node_modules

docs-dev:
	@cd docs && bunx mint@latest dev

docs-check:
	@cd docs && bunx mint@latest broken-links

smoke-ingest:
	@./hack/local-ingest-test.sh

smoke-mail:
	@cd deploy/mail-mbsync && ./smoke-test.sh

# Copy tracked hooks into .git/hooks. Idempotent; re-run after adding a
# new script under hooks/. Uses install -D so a fresh clone that
# doesn't yet have .git/hooks/ still works.
install-hooks:
	@set -e; for f in hooks/*; do \
	  case "$$f" in hooks/README.md) continue;; esac; \
	  install -D -m 0755 "$$f" .git/hooks/"$$(basename $$f)"; \
	  echo "installed .git/hooks/$$(basename $$f)"; \
	done

# Perf guardrail: rebuild suchi and re-measure idle RAM, cold start,
# goroutine count, binary size. Fails hard if any metric exceeds the
# `hard` threshold in hack/bench/thresholds.json.
bench-check: build
	@./hack/bench/bench.sh --scenario 01 --scenario 02 --scenario 03 --scenario 07 --thresholds

.PHONY: bench-check
