# suchi

**suchi** (Sanskrit *सूची*, "an index, a catalog, a list"; pronounced *SOO-chee*, like kimchi) — a document-management system as a single Go binary. SQLite by default, content-addressed storage, plugin seams at every layer, and wire-compatible with the an existing DMS mobile ecosystem.

Status: **pre-alpha** — Phases 0-3 shipped, Phase 4 (mobile compat + go-public) next. Not for production use. Repo is private until Phase 4.

## Non-negotiables

- One binary, one config, one data dir. No Redis, no Postgres for MVP.
- Idle RAM budget: ~100MB. Idle CPU: near-zero.
- **No telemetry, ever.** A stock install makes zero outbound connections. Every egress is opt-in, visible in `config.yaml`, and logged.
- AGPL-3.0. DCO/CLA once the repo goes public.

Full design lives at `../suchi-plan.md` (upstream design doc, out of tree). User-facing reference lives in `docs/` and is published via Mintlify. Brand assets live at `../design-lang/brand/`.

## Layout

```
suchi/
├── go.work                — workspace linking all modules
├── plugin-api/            — interfaces + shared types; the only dep every module shares
├── core/                  — HTTP, DB, jobs, audit, auth chain, pipeline, workflow engine, UI. Imports plugin-api only.
├── plugins/               — reference plugins, each its own module
│   ├── local-auth/
│   ├── oidc/
│   └── llm-classifier/
├── distro/                — the shipped binary. Pins versions, blank-imports enabled plugins.
│   └── cmd/suchi/         — main entry point (subcommands: serve, healthcheck, import, gc, taxonomy, doctor, version)
├── docs/                  — Mintlify MDX; published via docs.json
├── hack/                  — local dev harnesses (ingest fixtures, smoke scripts)
├── hooks/                 — git hooks (pre-commit gofmt)
├── Justfile               — dev shortcuts (`just serve`, `just fresh`, `just doctor`, etc.)
├── Dockerfile             — two targets: slim (distroless) + full (adds OCR/qpdf/poppler/djvulibre)
└── .github/workflows/     — ci.yml (per-module test/vet), smoke.yml (full-image end-to-end)
```

Every module has its own `go.mod`; `go.work` links them so `go build ./...` at the workspace root just works.

## Requirements

**Build**: Go 1.25+ (`plugin-api` targets 1.24 to stay maximally consumable).

**Runtime**: for the full ingest pipeline install the external tools you want active — each degrades gracefully when absent:

- `qpdf` — normalization (strip restrictions, decrypt empty-user-password PDFs)
- `pdftotext` + `pdftoppm` from `poppler-utils` — text-native shortcut in pdf-inspector; also drives the built-in `tessocr` OCR path and blank-page detection
- `tesseract-ocr` (+ language data) — required by the `tessocr` OCR engine (default in slim) and by `ocrmypdf`
- `ocrmypdf` — optional; produces a searchable-PDF archive in addition to text (default engine in the full image)
- `djvutxt` from `djvulibre-bin` — DjVu text extraction
- `ghostscript` — used by ocrmypdf and (in CI) to build the smoke fixture

The Docker `slim` image ships qpdf + poppler-utils + tesseract (full PDF pipeline via `tessocr`, ~70 MB). The `full` image adds ocrmypdf + djvulibre + libreoffice-core (~1 GB) — pick full when you want the searchable-PDF archive or DjVu ingest.

Run `suchi doctor` any time for a snapshot of which tools are on PATH, egress surface, and schema version.

## Development

### First-time setup

```sh
git clone git@github.com:johnnybravo-xyz/suchi.git
cd suchi
make install-hooks     # copies hooks/pre-commit → .git/hooks; runs gofmt on staged .go files
```

### Run the server locally

```sh
just serve             # preferred — boots against /tmp/suchi-dev, preserves data across restarts
# equivalent to:
#   PUBLIC_URL=http://127.0.0.1:8000 DATA_DIR=/tmp/suchi-dev LISTEN_ADDR=:8000 ./dist/suchi serve
```

Then visit `http://127.0.0.1:8000/`. On first boot suchi mints a one-time **setup token** (logged at WARN as `localauth.setup.token_minted`). Opening the browser drops you at `/bootstrap` — paste the token there and pick an admin email + password to finish setup, or POST it directly to `/setup`:

```sh
just setup-token       # prints the token from the running dev log
curl -X POST http://127.0.0.1:8000/setup \
  -H 'Content-Type: application/json' \
  -d '{"token":"...","email":"you@example.com","password":"..."}'
```

The setup token is only minted when the `users` table is empty — restart-safe:

```sh
just serve             # reboots against the same DB; keeps admin, docs, tokens
just fresh             # wipes DATA_DIR and starts clean (use after `just setup-token` fails)
just reset             # wipes DATA_DIR without booting
```

`just doctor` runs the same egress/binary/schema snapshot against a live instance on `:8000`.

### Full pipeline locally

Uploads land in the outbox and get picked up by the post-ingest handler. To exercise the whole chain (qpdf → pdftotext / OCR → ZUGFeRD → rules → render → LLM), install the binaries listed under Requirements and re-run `just serve`.

Optional LLM classifier (Phase 3, shipped): configure via the setup wizard's LLM step, or via env:

```sh
export LLM_ENDPOINT_URL=http://127.0.0.1:11434/v1   # e.g. Ollama on box
export LLM_MODEL=llama3
# non-local endpoints additionally require:
# export LLM_EGRESS_ACK=true
```

Empty `LLM_ENDPOINT_URL` keeps the classifier off (zero-egress default). Live-reload works — save settings via the wizard or `POST /api/admin/settings/llm` and the running classifier swaps to the new config on its next call.

### Workflow engine (Phase 5, shipped)

State-machine engine for review/approval chains. Definitions are JSON specs; runs advance through the durable outbox. Human approvals land in `/api/tasks/` alongside machine jobs so mobile clients poll one endpoint for both.

- `POST /api/workflows` — register a spec (admin)
- `POST /api/workflows/{slug}/start` — start a run
- `POST /api/workflows/tasks/{id}/resolve` — resolve a human task

See `docs/workflows.mdx` for the invoice-approval worked example.

### Plugin authoring

Three interfaces — `Subscriber`, `Authenticator`, `AuditSink` — cover most extension needs. See `docs/plugins.mdx` for a plugin authoring guide with worked examples (auto-tag-pdf, internal-JWT auth, syslog audit forwarder).

### Test / vet / lint

```sh
make test              # per-module `go test -count=1 ./...`
make vet
make lint              # staticcheck (auto-installs if missing)
make fmt               # gofmt -w on every .go file
make tidy              # `go mod tidy` in every module
make check             # fmt clean + lint clean + tests — same gate CI runs
```

The pre-commit hook runs `gofmt` and blocks the commit if anything's unformatted — CI's `gofmt` gate has bounced pushes before it landed, so leave the hook installed.

### Smoke test (local build sanity)

```sh
make smoke             # builds, boots on :8765, hits /healthz + /readyz, kills
just smoke-ingest      # boots suchi + drops .eml fixtures + asserts ingest pipeline
just smoke-mail        # runs the mail-mbsync docker recipe smoke test
```

For a full end-to-end run against real binaries, see `.github/workflows/smoke.yml` — easier to push and let CI run it than to reproduce the Docker + LaTeX-free PDF sample locally.

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
suchi healthcheck           # exits 0 iff /readyz answers 200 (used by Docker HEALTHCHECK)
suchi import bundle      # ingest a an existing DMS export bundle (see docs/importer.mdx)
suchi gc                    # mark-and-sweep blob reclamation
suchi taxonomy merge        # dedup tags/correspondents/types (see docs/cli.mdx)
suchi doctor                # diagnostic report — egress surface, binaries on PATH, schema version, DATA_DIR writability
suchi version               # build info
```

## CI

- `.github/workflows/ci.yml` — per-module `go test`, `go vet`, `gofmt` gate. Runs on every push + PR.
- `.github/workflows/smoke.yml` — builds the Docker `full` image, boots it against a throwaway volume, uploads a Ghostscript-generated text-native PDF, and asserts the pipeline extracted the right content via HTTP. This is the only thing that catches wrapper-vs-real-binary mismatches.

Both must be green before merging to `main`.
