.PHONY: build test vet lint fmt tidy run clean smoke

BIN := $(PWD)/dist/suchi
MODULES := plugin-api core plugins/local-auth plugins/oidc distro

build:
	@mkdir -p dist
	cd distro && CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o $(BIN) ./cmd/suchi
	@echo "built $(BIN) ($$(du -h $(BIN) | cut -f1))"

test:
	@for m in $(MODULES); do echo "=== test $$m ==="; ( cd $$m && go test -count=1 -timeout 60s ./... ) || exit 1; done

vet:
	@for m in $(MODULES); do echo "=== vet $$m ==="; ( cd $$m && go vet ./... ) || exit 1; done

lint:
	@command -v staticcheck >/dev/null || go install honnef.co/go/tools/cmd/staticcheck@latest
	staticcheck ./plugin-api/... ./core/... ./plugins/local-auth/... ./plugins/oidc/... ./distro/...

fmt:
	gofmt -w $$(find . -type f -name '*.go' -not -path './*/vendor/*')

tidy:
	@for m in $(MODULES); do echo "=== tidy $$m ==="; ( cd $$m && go mod tidy ) || exit 1; done

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
