<p align="center">
  <picture>
    <source media="(prefers-color-scheme: dark)" srcset=".github/brand/suchi-hero-dark.svg">
    <img src=".github/brand/suchi-hero.svg" alt="suchi" width="128">
  </picture>
</p>

<h1 align="center">suchi</h1>

<p align="center">
  <b>A document-management system as a single Go binary.</b><br>
  <sub>Sanskrit <i>सूची</i> — "an index, a catalog, a list"; pronounced <i>SOO-chee</i>, like kimchi.</sub>
</p>

<p align="center">
  <a href="https://github.com/johnnybravo-xyz/suchi/actions/workflows/ci.yml"><img alt="CI" src="https://github.com/johnnybravo-xyz/suchi/actions/workflows/ci.yml/badge.svg"></a>
  <a href="LICENSE"><img alt="License: AGPL-3.0" src="https://img.shields.io/badge/License-AGPL--3.0-007ec6"></a>
  <a href="https://suchi.page"><img alt="Homepage" src="https://img.shields.io/badge/site-suchi.page-007ec6"></a>
</p>

SQLite by default, content-addressed storage, plugin seams at every layer, and a mobile wire surface common DMS mobile clients can drive.

Status: **pre-alpha** — not for production use. The API + storage layout are stabilising but not frozen; migrations will apply cleanly across upgrades. See the [`suchi.page`](https://suchi.page) homepage for the pitch and [`docs/`](docs/) (published via Mintlify) for the full reference.

## Non-negotiables

- One binary, one config, one data dir. No Redis, no Postgres.
- Idle RAM budget: ~100 MB. Idle CPU: near-zero.
- **No telemetry, ever.** A stock install makes zero outbound connections. Every egress is opt-in, listed in the config, and logged.
- AGPL-3.0.
- Multi-user data model + groups + object ACLs from day one.
- Pure Go: `CGO_ENABLED=0`, single statically-linked binary, cross-platform out of the box.

Brand assets: [`.github/brand/`](.github/brand/) (hero + dark variants).

## What ships in the box

- **HTTP API** — ~140 routes covering documents, taxonomy, search, share links, automations, approvals, groups, ACLs, agents, MCP, mobile-compat, OpenAPI at `/api/schema/`.
- **Two UIs, one binary.** Server-rendered pages (list, detail, upload, inbox, admin: setup, mail-mbsync, automations, groups, custom fields) at `/` — Oat CSS + minimal JS. Svelte SPA at `/app/` — full-featured browser client, `//go:embed`'d from `core/ui/spa/dist/`. Both consume the same auth chain + API. `SUCHI_UI_DISABLED=1` disables both for headless.
- **Ingest pipeline** — 16 packages under `core/pipeline/` handling qpdf → pdf-inspector → OCR (tessocr / ocrmypdf) → anydoc (office docs) → eml / msg / epub / heic / djvu / zugferd / barcode / pageanalyze / docsplit → rules classifier → automations → rendered-view → optional LLM classifier.
- **Three ingest producers** — HTTP upload (`POST /api/documents/`), fs-watch (`core/ingest/fswatch`), email-watch (`core/ingest/emailwatch` — real IMAP polling loop, cred-managed via mail-mbsync sidecar).
- **Multi-language support** — per-doc `documents.languages` column, LLM-driven detection (opt-in), `?lang=` search filter, `/api/languages/` archive facet. Search preprocessor is Unicode-aware — Devanagari, Kannada, Tamil, CJK, Cyrillic queries all work. Detection is LLM-first in v1; interface seam at `core/lang.Detector` for future plugins (xberg-sidecar, lingua-go, etc.).
- **Two automation engines** — [automations](docs/automations.mdx) (trigger→conditions→actions, `document_added` / `document_updated` / `consumption`) and [approvals](docs/approvals.mdx) (human-in-the-loop state machines with timeouts). Ships a built-in `rescan-proposal` approval def that surfaces stale-pipeline docs to admins after a version bump.
- **Selective re-processing** — `suchi refile` re-runs the rules classifier + render job; `suchi rescan --stale <kind>` re-runs the full extraction chain (OCR / LLM / content) against docs whose signature lags the current binary. Signature bumps are code-driven per-kind — nothing runs unless the operator asks.
- **Permissions** — `groups` + `object_acls` + `Authorizer` interface. Default `ACLAuthorizer` — behaves as legacy "owner or admin" when no ACLs are set; unlocks per-user + per-group grants when they are.
- **Agents + MCP** — task-claim/act loop (`POST /api/tasks/…/claim`), HMAC-signed webhooks, and an MCP v2 adapter (`suchi mcp` — stdio for Claude Desktop, `--http` for remote runtimes).
- **Config file loader** — TOML (default), HUML, YAML, JSON. Env wins on collisions; search order: `--config` flag → `SUCHI_CONFIG` env → `$XDG_CONFIG_HOME/suchi/config.*` → `./suchi.toml`.
- **Portable export** — `suchi export --out FILE.zip` writes every doc's original bytes + JSON sidecars + taxonomy dumps into one archive. Rehydrates via `suchi import --from FILE.zip`.

Full feature list: [docs/comparison.mdx](docs/comparison.mdx) has the honest matrix vs Paperless-ngx, Papra, docspell.

## Layout

```
suchi/
├── go.work                — workspace linking all modules
├── plugin-api/            — interfaces + shared types; the only dep every module shares
├── core/                  — HTTP, DB, jobs, audit, auth chain, pipeline, workflow engine, UI. Imports plugin-api only.
│   ├── api/               — HTTP handlers (JSON surface)
│   ├── ui/                — server-rendered pages + assets + SPA embed at spa/dist/
│   ├── db/migrations/     — 29 embedded SQL migrations
│   ├── pipeline/          — 16 ingest processing steps
│   ├── ingest/            — 3 canonical producers (fswatch, emailwatch, sidecar spec)
│   ├── approvals/         — state-machine engine for human-in-the-loop chains
│   ├── automations/       — trigger→conditions→actions engine
│   ├── authz/             — Authorizer interface + RoleAuthorizer/ACLAuthorizer
│   ├── classify/rules/    — deterministic classifier
│   ├── lang/              — language-detection seam (Detector, Chain, metadata hints)
│   ├── rescan/            — signature-driven re-extract verb (CLI + approvals handler)
│   ├── refile/            — rules re-run + render enqueue verb
│   ├── render/            — Gonja storage-path renderer + moves audit
│   ├── settings/          — typed wrapper over settings k/v table
│   ├── jd/                — Johnny.Decimal presets + tree
│   └── ...                — audit, blob (CAS), config, jobs (outbox), sandbox, i18n, httpx, logx, mailsetup
├── plugins/               — reference plugins, each its own module
│   ├── local-auth/        — argon2id password login + scoped API tokens + first-boot setup token
│   ├── oidc/              — generic OIDC bearer + signed-cookie session
│   └── llm-classifier/    — OpenAI-compatible endpoint, egress-ack gate
├── distro/                — the shipped binary. Pins versions, blank-imports enabled plugins.
│   └── cmd/suchi/         — main entry point (subcommands below)
├── deploy/                — systemd unit, Caddy/nginx/Traefik snippets, k8s manifest, mail-mbsync compose
├── docs/                  — Mintlify MDX; published via docs.json
├── hack/                  — local dev harnesses (ingest fixtures, transcript recorder, smoke scripts)
├── hooks/                 — git hooks (pre-commit gofmt)
├── Justfile               — dev shortcuts (`just serve`, `just fresh`, `just doctor`, etc.)
├── Dockerfile             — two targets: slim (Alpine + tessocr + anydoc) + full (adds ocrmypdf + djvulibre + msgconvert)
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
- `msgconvert` from `libemail-outlook-message-perl` — Outlook `.msg` → RFC 822 conversion
- `imagemagick` — HEIC/HEIF → PDF conversion
- `ghostscript` — used by ocrmypdf and (in CI) to build the smoke fixture
- `anydoc` — static Rust binary compiled into both images; office-doc → Markdown extraction

The Docker `slim` image ships qpdf + poppler-utils + tesseract + anydoc (~80 MB). The `full` image adds ocrmypdf + djvulibre + msgconvert + imagemagick (~400 MB) — pick full when you want the searchable-PDF archive, DjVu, HEIC, or Outlook `.msg` support.

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

Then visit `http://127.0.0.1:8000/`. On first boot suchi mints a one-time **setup token** (logged at WARN as `localauth.setup.token_minted`). Opening the browser drops you at `/bootstrap` — paste the token there and pick an admin email + password to finish setup, or POST it directly:

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

### Kicking tires with the demo dataset

```sh
suchi demo             # or: just demo
```

Seeds `$DATA_DIR` with sample docs, tags, correspondents, one automation, one rule. Idempotent — re-run without clobbering existing data. Skips user creation when there are already users. See `distro/cmd/suchi/demo.go` for the exact seed set.

### Full pipeline locally

Uploads land in the outbox and get picked up by the post-ingest handler. To exercise the whole chain (qpdf → pdftotext / OCR → anydoc → ZUGFeRD → rules → automations → render → LLM), install the binaries listed under Requirements and re-run `just serve`.

Optional LLM classifier: configure via the setup wizard's LLM step, or via env:

```sh
export LLM_ENDPOINT_URL=http://127.0.0.1:11434/v1   # e.g. Ollama on box
export LLM_MODEL=llama3
# non-local endpoints additionally require:
# export LLM_EGRESS_ACK=true
```

Empty `LLM_ENDPOINT_URL` keeps the classifier off (zero-egress default). Live-reload works — save settings via the wizard or `POST /api/admin/settings/llm` and the running classifier swaps to the new config on its next call.

### Automations (Phase 5)

Trigger→conditions→actions engine. On events (`consumption`, `document_added`, `document_updated`), evaluate optional filters (path glob, filename glob, tag, correspondent, document_type, content regex, mail-rule id), then run actions in order (assign_title, assign_tags, assign_correspondent, assign_document_type, assign_storage_path, assign_owner, assign_custom_field, remove_*). See [`docs/automations.mdx`](docs/automations.mdx).

- `GET/POST /api/automations/` — CRUD
- `/admin/automations` — UI

### Approvals (state-machine engine)

Human-in-the-loop chains. Definitions are JSON specs; runs advance through the durable outbox. Approval tasks land in `/api/tasks/` alongside machine jobs. See [`docs/approvals.mdx`](docs/approvals.mdx).

- `POST /api/approvals` — register a spec (admin)
- `POST /api/approvals/{slug}/start` — start a run
- `POST /api/approvals/tasks/{id}/resolve` — resolve a human task

Built-in def: `rescan-proposal` — after a `pipeline_version_*` bump, boot-time detection opens a card in every admin's inbox with **approve all / approve sample / dismiss** choices. Approvals route through the same `core/rescan.Enqueue` the CLI uses.

### Selective re-processing (refile / rescan)

Two verbs for coming back and changing your mind — both fully explicit, both signature-aware:

- **`suchi refile`** — re-run the rules classifier + enqueue a render/move job for every live doc. Use after preset / template / rule edits. Cheap.
- **`suchi rescan --stale <ocr|llm|content>`** — re-run the full extraction chain against docs whose `pipeline_version_<kind>` lags the current binary. Use after swapping OCR engine, LLM model, or content-pipeline shape. Expensive but authoritative. Filter by tag / correspondent / JD / date / `--sample N` / `--dry-run` / `--estimate`. See [`docs/cli.mdx#suchi-rescan-flags`](docs/cli.mdx).

### Permissions (Phase 6)

Groups + per-object ACLs. `object_acls(object_kind, object_id, principal_kind, principal_id, perm_bits)` behind the `Authorizer` interface. Empty ACLs = current owner+admin behavior; grants unlock per-user or per-group visibility. See [`docs/permissions.mdx`](docs/permissions.mdx).

- `GET/POST/PATCH/DELETE /api/groups/` — group CRUD
- `POST/DELETE /api/groups/{id}/members` — membership
- `GET/PUT/DELETE /api/acls/{kind}/{id}` — grant management
- `/admin/groups` — UI

### Plugin authoring

Three interfaces — `Subscriber`, `Authenticator`, `AuditSink` — cover most extension needs. See [`docs/plugins.mdx`](docs/plugins.mdx) for a plugin authoring guide with worked examples.

### MCP v2 adapter

```sh
suchi mcp              # stdio (Claude Desktop, Cursor)
suchi mcp --http :7000 # HTTP+SSE for remote runtimes
```

The binary also responds to `suchi-mcp` when invoked via symlink — Claude Desktop configs stay clean. Auth via `SUCHI_URL` + `SUCHI_TOKEN` env or `--url` / `--token` flags. Five tools: `search_documents`, `get_document`, `list_inbox`, `resolve_approval_task`, `create_share_link`. See [`docs/mcp.mdx`](docs/mcp.mdx).

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
just smoke-anydoc      # boots suchi + drops .docx fixture + asserts anydoc extraction
just smoke-mail        # runs the mail-mbsync docker recipe smoke test
```

For a full end-to-end run against real binaries, see `.github/workflows/smoke.yml`.

## Serving the docs

Docs are Mintlify MDX under `docs/`, indexed by `docs/docs.json`. Mintlify resolves page paths relative to `docs.json`, so **run the CLI from inside `docs/`**:

```sh
cd docs
bunx mint dev             # or: npx mint@latest dev (Node) — serves on http://127.0.0.1:3000
bunx mint broken-links    # validate before pushing
```

Production docs deploy is Mintlify-hosted (zero config beyond `docs.json`).

## Deployment

Copy-and-edit templates for the four common self-host shapes live under [`deploy/`](deploy/):

- `deploy/systemd/suchi.service` — hardened unit for a bare-binary Linux install
- `deploy/caddy/Caddyfile` — auto-TLS reverse proxy
- `deploy/nginx/suchi.conf` — bring-your-own-certs reverse proxy
- `deploy/traefik/suchi.yml` — dynamic-config snippet
- `deploy/k8s/suchi.yaml` — single-replica Deployment + PVC + Service (SQLite is single-writer; do NOT scale)
- `deploy/mail-mbsync/` — docker-compose IMAP-bridge sidecar

Every template documents the placeholders. See [`deploy/README.md`](deploy/README.md) for the one-liner-each intro.

### Docker

Two image targets in `Dockerfile`:

- `slim` — Alpine + qpdf + poppler-utils + tesseract + anydoc. ~80 MB. Full PDF pipeline including OCR of scanned pages (via `tessocr` — pdftoppm | tesseract) plus office-document text extraction (docx, xlsx, pptx, odt, rtf, csv) via anydoc.
- `full` — Debian slim + everything in slim + ocrmypdf + djvulibre-bin + msgconvert + imagemagick. ~400 MB. Adds text-selectable scanned-PDF archives, DjVu extraction, HEIC/HEIF conversion, and Outlook `.msg` parsing.

Both images accept `OCR_ENGINE={auto,tesseract,ocrmypdf}`. Slim defaults to `tesseract`; full defaults to `ocrmypdf`.

```sh
docker build --target full -t suchi:local .
docker run -d --name suchi \
  -p 8000:8000 -e PUBLIC_URL=http://127.0.0.1:8000 \
  -v suchi-data:/data suchi:local
docker logs -f suchi        # grab the setup token
```

Both images pre-own `/data` as UID 65532; named-volume or empty-bind mounts inherit that ownership.

### Direct binary

Build once, ship the artifact:

```sh
make build       # produces dist/suchi, statically linked, CGO_ENABLED=0
./dist/suchi serve
```

Runtime env vars: see [`docs/config.mdx`](docs/config.mdx) (comprehensive). `PUBLIC_URL` is the only required setting. `DATA_DIR` defaults to `/data`, `LISTEN_ADDR` to `:8000`.

Backing services: none. SQLite lives at `$DATA_DIR/suchi.db`, blobs at `$DATA_DIR/blobs/sha256/…`, rendered symlinks under `$DATA_DIR/rendered/`.

### Reverse proxy

Put nginx / caddy / traefik in front and terminate TLS there. `PUBLIC_URL` must match what the proxy exposes — it drives cookie domain, OIDC callbacks, and share-link URLs. Behind a proxy, leave `TLS_CERT_FILE` and `TLS_KEY_FILE` unset.

Direct-to-internet installs can set both `TLS_CERT_FILE` and `TLS_KEY_FILE` to serve HTTPS from suchi itself.

### Backups and restore

Full details in [`docs/backup-restore.mdx`](docs/backup-restore.mdx). Short version:

- Back up `$DATA_DIR` — SQLite database, `blobs/`, `.decrypt-key`, `mail-*.env`.
- `VACUUM INTO $DATA_DIR/backups/suchi-<ts>.db` runs on the interval `BACKUP_INTERVAL` (default `24h`, `0` disables).
- **Never** `cp suchi.db` while suchi runs — use `sqlite3 .backup`, stop-and-tar, or a filesystem snapshot.
- Blobs are content-addressed so a filesystem snapshot of `$DATA_DIR` is consistent as long as the SQLite file is captured atomically.

## Subcommands

```
suchi serve                       # HTTP server + job dispatcher (PUBLIC_URL required)
suchi healthcheck                 # probe /readyz on LISTEN_ADDR (for Docker HEALTHCHECK)
suchi import --from ./export      # ingest a bundle from your existing DMS (docs/importer.mdx)
suchi export --out FILE.zip       # portable takeout of every live doc + taxonomy dump
suchi gc [--older-than 30d]       # mark-and-sweep blob reclamation (dry-run default)
suchi taxonomy merge [flags]      # dedup tag/correspondent/document_type
suchi refile [flags]              # re-run rules classifier + enqueue render for every live doc
suchi rescan --stale <kind> ...   # signature-driven re-extract (OCR/LLM/content) with filters
suchi doctor [--json]             # diagnostic report — egress, binaries, schema, filesystem
suchi mcp [--http :port]          # MCP v2 server (stdio default, HTTP+SSE with --http)
suchi demo [--data-dir DIR]       # seed DATA_DIR with sample docs + one automation + one rule
suchi version                     # print version + build info
```

Every subcommand has full flag documentation in [`docs/cli.mdx`](docs/cli.mdx).

## CI

- `.github/workflows/ci.yml` — per-module `go test`, `go vet`, `gofmt` gate. Runs on every push + PR.
- `.github/workflows/smoke.yml` — builds the Docker `full` image, boots it against a throwaway volume, uploads a Ghostscript-generated text-native PDF, and asserts the pipeline extracted the right content via HTTP.

Both must be green before merging to `main`.
