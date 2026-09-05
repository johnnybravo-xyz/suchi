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
  <a href="LICENSE"><img alt="License: AGPL-3.0" src="https://img.shields.io/badge/License-AGPL--3.0-007ec6"></a>
  <a href="https://suchi.page"><img alt="Homepage" src="https://img.shields.io/badge/site-suchi.page-007ec6"></a>
</p>

Suchi combines SQLite, content-addressed storage, a Svelte interface, and an
integration-friendly HTTP API. It needs no database server, queue, cache, or
telemetry service.

Status: **v0.1 beta candidate**, suitable for evaluation but not yet critical
archives. The API and storage layout are stabilising but not frozen.

[Container images](https://github.com/johnnybravo-xyz/suchi/pkgs/container/suchi) ·
[Releases](https://github.com/johnnybravo-xyz/suchi/releases) ·
[Actions](https://github.com/johnnybravo-xyz/suchi/actions)

- Reproducible size and startup measurements live under
  [`hack/bench/latest-published/`](hack/bench/latest-published/).
- No built-in outbound connection is made until an operator enables or uses an
  integration. Operator scripts are outside Suchi's egress inventory.
- Static, `CGO_ENABLED=0` Go binary with multi-user ACLs and scoped tokens.
- AGPL-3.0.

## Capabilities

- PDF and image OCR, thumbnails, barcodes, encrypted PDFs, and ZUGFeRD invoices.
- EPUB, Office, OpenDocument, RTF, CSV, DjVu, HEIC/HEIF, EML, and Outlook MSG.
- Full-text search, Johnny.Decimal filing, custom fields, saved views, and
  rendered filesystem views.
- Browser uploads, watched folders, IMAP intake, portable import/export, and
  versioned documents.
- Automations, human approval workflows, selective rescans, and optional
  OpenAI-compatible classification, grounded archive research, and reviewed
  date intelligence with Calendar.
- Groups, object ACLs, OIDC, share links, audit events, backups, and restore
  tooling.
- Svelte SPA, a documented scoped HTTP API, and MCP over stdio or HTTP.

The [feature comparison](docs/comparison.mdx) and
[architecture](docs/architecture.mdx) describe the detailed scope and
tradeoffs.

## Quick Start

The unsuffixed image is the recommended standard build:

```sh
docker volume create suchi-data
docker run -d --name suchi --restart unless-stopped \
  -p 8000:8000 \
  -e PUBLIC_URL=http://127.0.0.1:8000 \
  -v suchi-data:/data \
  ghcr.io/johnnybravo-xyz/suchi:beta
docker logs suchi 2>&1 | grep token_minted
```

Open `http://127.0.0.1:8000`, enter the one-time setup token, and create the
first admin account. The token expires after 24 hours; attempting to use an
expired token writes a replacement to the server log. A minimal
[`compose.yaml`](compose.yaml) is also provided
for operators who want editable mounts, networks, and image pins. See [Getting
started](docs/getting-started.mdx) for direct binary, reverse-proxy, NAS, and
production deployment paths.

## Images

- `standard` / `beta` / `beta-standard`: Alpine. Supports all listed formats
  and indexes scanned PDFs with Tesseract. The two beta tags resolve to the
  same image.
- `full` / `beta-full`: Debian. Adds OCRmyPDF so downloaded scanned PDFs can
  retain a searchable text layer.

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
distro/       shipped suchi and suchi-mcp entry points
ui/           Svelte SPA source
deploy/       self-hosting templates and mail intake sidecar
docs/         published documentation source
hack/         fixtures, benchmarks, smoke tests, and developer tools
```

The root module contains the shipped application; `plugin-api` stays separate
for external plugins and the utilities under `hack/` keep isolated dependency
graphs. The empty `ui` module keeps Go tooling out of frontend dependencies.
`make build` produces `dist/suchi`; `suchi doctor` inventories configured
egress, pipeline tools, schema and taxonomy state, data-directory writability,
and selected job, backup, audit, upload-limit, and CAS indicators.

## Documentation

- [Documentation](https://docs.suchi.page)
- [Configuration](docs/config.mdx)
- [CLI reference](docs/cli.mdx)
- [HTTP API](docs/api.mdx)
- [Supported file types](docs/formats.mdx)
- [Deployment templates](deploy/README.md)
- [Backup and restore](docs/backup-restore.mdx)
- [Contributing](CONTRIBUTING.md)
- [Security policy](SECURITY.md)

Works with [Johnny.Decimal](https://johnnydecimal.com), a trademark of
Coruscade Pty Ltd. Suchi is independent and not endorsed by them.

## License

Suchi is made available under the [GNU Affero General Public License v3.0](LICENSE)
beginning with its first public release. Before that release, its repository
and container images were private development artifacts and were not
distributed to any third party.
