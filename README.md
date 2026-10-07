<p align="center">
  <picture>
    <source media="(prefers-color-scheme: dark)" srcset=".github/brand/suchi-hero-dark.svg">
    <img src=".github/brand/suchi-hero.svg" alt="suchi" width="128">
  </picture>
</p>

<h1 align="center">suchi</h1>

<p align="center">
  <b>A self-hosted document archive that files itself.</b><br>
  <sub>Sanskrit <i>सूची</i> — "an index, a catalog, a list"; pronounced <i>SOO-chee</i>, like kimchi.</sub>
</p>

<p align="center">
  <a href="LICENSE"><img alt="License: AGPL-3.0" src="https://img.shields.io/badge/License-AGPL--3.0-007ec6"></a>
  <a href="https://suchi.page"><img alt="Homepage" src="https://img.shields.io/badge/site-suchi.page-007ec6"></a>
</p>

<p align="center">
  <a href="https://demo.suchi.page">Live demo</a> ·
  <a href="#quick-start">Quick start</a> ·
  <a href="https://docs.suchi.page">Docs</a> ·
  <a href="https://johnnybravo.xyz/posts/building-suchi-dms/">Why I built it</a>
</p>

<p align="center">
  <img src=".github/brand/suchi-demo.gif" alt="A bank statement arrives by email and is filed; then IMG_2047.pdf is dropped in, read, renamed Form 16 · FY 2025-26 and filed under 23 Taxes." width="800">
</p>

Drop in a scan, an email attachment or a photo. Suchi reads it, gives it
a useful title and files it into a numbered category tree. If it is unsure, it
leaves the document in the Inbox for you instead of guessing. Originals are
never modified, and the archive is also rendered as a plain folder tree you can
browse without the app.

Suchi began as a personal and household archive. The same principles apply more
broadly, so the built-in filing trees also include approachable starting points
for freelancers and smaller teams.

- **One binary.** Go, SQLite and files on disk. No Postgres, Redis, queue or
  telemetry service. Nothing calls out until you enable an integration.
- **One filing model.** Numbered categories in the
  [Johnny.Decimal](https://johnnydecimal.com) style instead of overlapping tags,
  types and storage paths. Start from a built-in tree or import your own.
- **Deterministic automations first.** Suchi calls its explicit text-matching
  rules automations; they run before any model. An optional model, either local
  (Ollama, llama.cpp) or any OpenAI-compatible endpoint, suggests titles,
  categories and dates. Sending document text to a hosted model requires
  explicit acknowledgement.
- **Capture from anywhere.** Browser upload, watched folders, your own IMAP
  mailbox, and the [Suchi Companion](https://github.com/johnnybravo-xyz/suchi-companion)
  app for iOS and Android, which can queue and keep scans while offline.
- **Find it and follow up.** OCR and full-text search, a calendar of dates
  linked to the text they came from, and archive questions answered with links
  to the source documents.

> [!NOTE]
> Suchi is early (v0.1.0). Back up the data
> directory, test a restore before relying on it, and read the
> [database compatibility policy](docs/release-process.mdx#stable-v1-database-compatibility)
> before upgrading.

## How it is built

Suchi combines SQLite, content-addressed storage, a Svelte interface, and an
integration-friendly HTTP API. Original and derived bytes are stored
immutably under their SHA-256 digest, so identical files share one object and
retried uploads do not create copies. The content store is not encryption or an
access-control boundary; SQLite metadata and the API decide who can see each
document.

- Static, `CGO_ENABLED=0` Go binary with multi-user ACLs and scoped tokens.
- Reproducible size and startup measurements live under
  [`hack/bench/latest-published/`](hack/bench/latest-published/).
- [Container images](https://github.com/johnnybravo-xyz/suchi/pkgs/container/suchi) ·
  [Releases](https://github.com/johnnybravo-xyz/suchi/releases) ·
  [Actions](https://github.com/johnnybravo-xyz/suchi/actions)

## Capabilities

- PDF and image OCR, thumbnails, barcodes, encrypted PDFs, and ZUGFeRD invoices.
- EPUB, Office, OpenDocument, RTF, CSV, DjVu, HEIC/HEIF, EML, and Outlook MSG.
- Full-text search, Johnny.Decimal filing, revision notes, typed custom fields,
  named exact-document links, saved views, and folder layouts.
- Import-introduced filing systems with permanent codes, direct membership plus
  document ACLs, and `SYS.AC.documentID` addresses using existing global IDs.
  Unprefixed archives keep their existing UI and paths until first prefixed Apply.
- Browser uploads, watched folders, IMAP intake, portable import/export, and
  versioned documents.
- Automations, human approval workflows, selective rescans, and optional
  OpenAI-compatible classification, grounded archive research, and source-backed
  date intelligence with Calendar. High-confidence suggestions apply by default;
  review-first mode sends inferred changes to Approvals instead.
- Groups, object ACLs, OIDC, share links, audit events, backups, and restore
  tooling.
- Svelte SPA, [Suchi Companion](https://github.com/johnnybravo-xyz/suchi-companion)
  for Android/iOS, a documented scoped HTTP API, and MCP over stdio or HTTP.

The [feature comparison](docs/comparison.mdx) and
[architecture](docs/architecture.mdx) describe the detailed scope and
tradeoffs.

## Quick Start

The versioned standard image is a convenient local evaluation starting point:

```sh
docker volume create suchi-data
docker run -d --name suchi --restart unless-stopped \
  -p 127.0.0.1:8000:8000 \
  -e PUBLIC_URL=http://127.0.0.1:8000 \
  -v suchi-data:/data \
  ghcr.io/johnnybravo-xyz/suchi:v0.1.0
docker logs suchi 2>&1 | grep token_minted
```

Open `http://127.0.0.1:8000`, enter the one-time setup token, and create the
first admin account. The token expires after 24 hours; attempting to use an
expired token writes a replacement to the server log. A minimal
[`compose.yaml`](compose.yaml) is also provided
for operators who want editable mounts, networks, and image pins. See [Getting
started](docs/getting-started.mdx) for direct binary, reverse-proxy, NAS, and
production deployment paths.

After creating the administrator, choose a filing tree in **Settings > Archive
configuration > Filing tree**. This is the only required archive setup step;
the reminder remains until a preset, imported tree, or explicit Blank choice is
saved.

This quick start is bound to loopback and pins the v0.1.0 image. A production
server, including one reached by the mobile app, must use HTTPS and pin the
selected release by its published image digest.

## Images

- `v0.1.0` / `latest`: Alpine. Supports all listed formats and indexes scanned
  PDFs with Tesseract.
- `v0.1.0-full` / `latest-full`: Debian. Adds OCRmyPDF so downloaded scanned
  PDFs can retain a searchable text layer.

Both images include anydoc, DjVu, HEIC/HEIF, and Outlook MSG support. The full
image changes only the scanned-PDF archive behavior. See [Supported file
types](docs/formats.mdx) for the exact routing and bare-metal dependencies.

## Development

The workspace requires Go 1.27.0 or newer; `plugin-api` remains compatible
with Go 1.24.
SPA and documentation development require Bun.

```sh
git clone https://github.com/johnnybravo-xyz/suchi.git
cd suchi
make install-hooks
make check
make run
```

`make run` uses `/tmp/suchi-dev` and listens on `http://127.0.0.1:8000`.
Useful verification commands:

```sh
make test
make lint
make ui-check
make smoke
make smoke-ingest
./hack/smoke-anydoc-docx.sh
make smoke-mail
```

The `ci` workflow runs formatting, vet, tests, static analysis, and the SPA
build. The `smoke` workflow boots both images and verifies real PDF
OCR and Outlook MSG ingestion.

## Repository

```text
plugin-api/   shared extension interfaces and types
core/         API, database, ingest pipeline, jobs, auth, and embedded UI
plugins/      local auth, OIDC, and LLM classifier modules
distro/       importable app assembly and shipped suchi / suchi-mcp entry points
ui/           Svelte SPA source
deploy/       self-hosting templates and mail intake sidecar
docs/         published documentation source
hack/         fixtures, benchmarks, smoke tests, and developer tools
```

The root module contains the shipped application; `plugin-api` stays separate
for external plugins and the utilities under `hack/` keep isolated dependency
graphs. The empty `ui` module keeps Go tooling out of frontend dependencies.
`make build` uses Bun to install locked frontend dependencies, generates the
ignored SPA bundle, and produces `dist/suchi`; `suchi doctor` inventories
configured egress, pipeline tools, schema and taxonomy state, data-directory
writability, and selected job, backup, audit, upload-limit, and CAS indicators.

## Documentation

- [Documentation](https://docs.suchi.page)
- [Configuration](docs/config.mdx)
- [CLI reference](docs/cli.mdx)
- [HTTP API](docs/api.mdx)
- [Filing systems and taxonomy imports](docs/jd.mdx)
- [Supported file types](docs/formats.mdx)
- [Deployment templates](deploy/README.md)
- [Backup and restore](docs/backup-restore.mdx)
- [Contributing](CONTRIBUTING.md)
- [Security policy](SECURITY.md)

Works with [Johnny.Decimal](https://johnnydecimal.com), a trademark of
Coruscade Pty Ltd. Suchi is independent and not endorsed by them.
The built-in filing trees are CC0-1.0, unreviewed and not compliance advice. Filing systems
share one server, SQLite/FTS and CAS; server administrators and host/backup operators
remain trusted. A code prefix is not separate infrastructure or a legal-independence
guarantee. See [Permissions](docs/permissions.mdx) for the application boundary.

## License

Copyright (c) 2026 Ritesh Shrivastav. Suchi is available under two licenses:

- **[GNU Affero General Public License v3.0](LICENSE)** — free for everyone.
  Note that if you modify Suchi and let others interact with it over a network,
  AGPL section 13 requires you to offer them its complete corresponding source.
- **Commercial license** — for embedding Suchi in a proprietary product, or for
  running a modified instance as a service without that source-offer obligation.
  Write to <contact@suchi.page>.

The AGPL offer begins with Suchi's first public release. Before that release,
its repository and container images were private development artifacts and were
not distributed to any third party.

The plugin interfaces in [`plugin-api/`](plugin-api/) are a separate module
licensed under the [Apache License 2.0](plugin-api/LICENSE), so third-party
plugins may be released under any license, including proprietary ones.

Third-party components and their terms are listed in [NOTICE](NOTICE).
Contributions are accepted under the [Contributor License Agreement](CLA.md).
