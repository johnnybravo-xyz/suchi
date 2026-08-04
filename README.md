# suchi

**suchi** (Sanskrit *सूची*, "an index, a catalog, a list") — a document-management system as a single Go binary. SQLite by default, content-addressed storage, plugin seams at every layer, and wire-compatible with the Paperless-ngx mobile ecosystem.

Status: **pre-alpha** — Phase 2 shipping. Not for production use. Repo is private until Phase 4.

## Non-negotiables

- One binary, one config, one data dir. No Redis, no Postgres for MVP.
- Idle RAM budget: ~100MB. Idle CPU: near-zero.
- **No telemetry, ever.** A stock install makes zero outbound connections. Every egress is opt-in, visible in `config.yaml`, and logged.
- AGPL-3.0. DCO/CLA once the repo goes public.

Full design lives in `notes/suchi-plan.md` (upstream design doc, out of tree). User-facing reference lives in `docs/` and is published via Mintlify.

## Layout

```
suchi/
├── go.work                — workspace linking all modules
├── plugin-api/            — interfaces + shared types; the only dep every module shares
├── core/                  — HTTP, DB, jobs, audit, auth chain, pipeline. Imports plugin-api only.
├── plugins/               — reference plugins, each its own module
│   ├── local-auth/
│   ├── oidc/
│   └── llm-classifier/
├── distro/                — the shipped binary. Pins versions, blank-imports enabled plugins.
│   └── cmd/suchi/         — main entry point (subcommands: serve, healthcheck, import, gc, taxonomy)
├── docs/                  — Mintlify MDX; published via docs.json
├── hooks/                 — git hooks (pre-commit gofmt)
├── Dockerfile             — two targets: slim (distroless) + full (adds OCR/qpdf/poppler/djvulibre)
└── .github/workflows/     — ci.yml (per-module test/vet), smoke.yml (full-image end-to-end)
```

Every module has its own `go.mod`; `go.work` links them so `go build ./...` at the workspace root just works.

## Requirements

**Build**: Go 1.25+.

**Runtime**: none for the slim binary. For the full ingest pipeline install the external tools you want active — each degrades gracefully when absent:

- `qpdf` — normalization (strip restrictions, decrypt empty-user-password PDFs)
- `pdftotext` from `poppler-utils` — text-native shortcut in pdf-inspector
- `ocrmypdf` + `tesseract-ocr` — scanned-PDF path; emits searchable archive + text sidecar
- `djvutxt` from `djvulibre-bin` — DjVu text extraction
- `ghostscript` — used by ocrmypdf and (in CI) to build the smoke fixture

The Docker `full` target ships all of them; the `slim` target ships none.

## Development

### First-time setup

```sh
git clone git@github.com:suchi-dms/suchi.git
cd suchi
make install-hooks     # copies hooks/pre-commit → .git/hooks; runs gofmt on staged .go files
```

### Run the server locally

```sh
make run
# equivalent to:
#   PUBLIC_URL=http://127.0.0.1:8000 DATA_DIR=/tmp/suchi-dev ./dist/suchi serve
```

Then visit `http://127.0.0.1:8000/`. On first boot suchi prints a one-time **setup token** at WARN level in the log; POST it to `/setup` with an email + password to create the admin.

### Full pipeline locally

Uploads land in the outbox and get picked up by the post-ingest handler. To exercise the whole chain (qpdf → pdftotext / OCR → ZUGFeRD → rules → render → LLM), install the binaries listed under Requirements and re-run `make run`.

Optional LLM classifier (Phase 3): set

```sh
export LLM_ENDPOINT_URL=http://127.0.0.1:11434/v1   # e.g. Ollama on box
export LLM_MODEL=llama3
# non-local endpoints additionally require:
# export LLM_EGRESS_ACK=true
```

before `make run`. Empty `LLM_ENDPOINT_URL` keeps the classifier off (zero-egress default).

### Test / vet / lint

```sh
make test         # per-module `go test -count=1 ./...`
make vet
make lint         # staticcheck (auto-installs if missing)
make fmt          # gofmt -w on every .go file
make tidy         # `go mod tidy` in every module
```

The pre-commit hook runs `gofmt` and blocks the commit if anything's unformatted — CI's `gofmt` gate has bounced pushes before it landed, so leave the hook installed.

### Smoke test (local build sanity)

```sh
make smoke        # builds, boots on :8765, hits /healthz + /readyz, kills
```

For a full end-to-end run against real binaries, see `.github/workflows/smoke.yml` — it's easier to push and let CI run it than to reproduce the Docker + LaTeX-free PDF sample locally.

## Serving the docs

Docs are Mintlify MDX under `docs/`, indexed by `docs/docs.json`. Mintlify resolves page paths relative to `docs.json`, so **run the CLI from inside `docs/`**:

```sh
cd docs
bunx mint dev             # or: npx mint@latest dev (Node) — serves on http://127.0.0.1:3000
```

The dev server watches `.mdx` files and hot-reloads on save. To validate before pushing:

```sh
cd docs
bunx mint broken-links
```

Production docs deploy is Mintlify-hosted (zero config beyond `docs.json` — see the Mintlify dashboard for the deploy pipeline).

## Deployment

### Docker

Two image targets in `Dockerfile`:

- `slim` — Alpine + qpdf + poppler-utils + tesseract. ~70 MB. Full PDF pipeline including OCR of scanned pages (via the in-process `tessocr` engine — `pdftoppm | tesseract`). No searchable-PDF archive, no DjVu, no LibreOffice.
- `full` — Debian slim + tesseract + ocrmypdf + qpdf + poppler-utils + djvulibre-bin + libreoffice-core. ~1 GB. Adds ocrmypdf (searchable-PDF archives) and the other converters.

Both images accept `OCR_ENGINE={auto,tesseract,ocrmypdf}`. Slim defaults to `tesseract`; full defaults to `ocrmypdf`.

Build + run:

```sh
docker build --target full -t suchi:local .
docker run -d \
  --name suchi \
  -p 8000:8000 \
  -e PUBLIC_URL=http://127.0.0.1:8000 \
  -v suchi-data:/data \
  suchi:local
docker logs -f suchi        # grab the setup token from a `localauth.setup.token_minted` line
```

Both images pre-own `/data` as UID 65532; named-volume or empty-bind mounts inherit that ownership so the non-root process can create `dms.db` on first boot without an entrypoint chown dance.

### Direct binary

Build once, ship the artifact:

```sh
make build       # produces dist/suchi, statically linked, CGO_ENABLED=0
./dist/suchi serve
```

Runtime env vars: see `docs/config.mdx` (comprehensive) or `PUBLIC_URL` at minimum — that's the only required setting. `DATA_DIR` defaults to `/data`, `LISTEN_ADDR` to `:8000`.

Backing services: none. SQLite lives at `$DATA_DIR/dms.db`, blobs at `$DATA_DIR/blobs/sha256/…`, rendered symlinks under `$DATA_DIR/rendered/`.

### Reverse proxy

Put nginx / caddy / traefik in front and terminate TLS there. `PUBLIC_URL` must match what the proxy exposes — it drives cookie domain, OIDC callbacks, and share-link URLs. Behind a proxy, leave `TLS_CERT_FILE` and `TLS_KEY_FILE` unset.

Direct-to-internet installs can set both `TLS_CERT_FILE` and `TLS_KEY_FILE` to serve HTTPS from suchi itself.

### Backups

`VACUUM INTO $DATA_DIR/backups/dms-<ts>.db` runs on the interval `BACKUP_INTERVAL` (default `24h`, `0` disables). Blobs are content-addressed so a filesystem-level snapshot of `$DATA_DIR` is consistent as long as the SQLite file is captured atomically (restic / borg / zfs snapshot are all fine).

## Subcommands

```
suchi serve                 # HTTP server + job dispatcher
suchi healthcheck           # exits 0 iff /healthz answers 200 (used by Docker HEALTHCHECK)
suchi import <flags>        # ingest a Paperless-ngx manifest (see docs/importer.mdx)
suchi gc                    # mark-and-sweep blob reclamation
suchi taxonomy merge <...>  # dedup tags/correspondents/types (see docs/cli.mdx)
suchi version
```

## CI

- `.github/workflows/ci.yml` — per-module `go test`, `go vet`, `gofmt` gate. Runs on every push + PR.
- `.github/workflows/smoke.yml` — builds the Docker `full` image, boots it against a throwaway volume, uploads a Ghostscript-generated text-native PDF, and asserts the pipeline extracted the right content via HTTP. This is the only thing that catches wrapper-vs-real-binary mismatches.

Both must be green before merging to `main`.
