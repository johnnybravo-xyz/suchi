# Security policy

## Reporting a vulnerability

**Please do NOT open a public GitHub issue for security bugs.**

Open a private issue via the repository security tab (**Security →
Advisories → Report a vulnerability**) — the maintainer receives
the notification directly.

Include, if you can:

- A short description of the issue and the impact you can reach
  with it.
- Steps to reproduce (curl command, sample document, config snippet).
- Suchi version affected — `suchi version` output or a git rev.
- Whether you've already coordinated with any other party.
- Any suggested patch or mitigation (optional).

**Encryption**: PGP key pending. When it lands, the fingerprint will
be published here and mirrored on keys.openpgp.org.

## What to expect

- **Acknowledgement** — a real human, not a bot,
  confirming the report is received.
- **Triage** — reproduction + severity assessment.
- **Fix + disclosure timeline** — we agree on a
  target with the reporter.
- **Public disclosure** happens after a patched release is out. We
  credit the reporter unless asked otherwise.

If a report turns out to be a non-issue, we explain why. Reports
are read carefully; even "this turns out to be by design" comes
with a written reasoning.

## Scope

In scope:

- **The `suchi` binary** in every shipped configuration (slim and
  full Docker images, direct-binary installs).
- **The plugin-api boundary** — anything an ABI-compliant plugin
  could reach.
- **The auth chain** — Local, OIDC, session cookies, API tokens.
- **The HTTP surface** — every route under `/api/*`, `/s/*`,
  `/healthz`, `/readyz`, `/metrics`, `/assets/*`, `/admin/*`.
- **The setup wizard** — including any state persisted to the
  settings table.
- **The rendered-view symlink structure** and its move audit trail.
- **The audit log** — completeness + tamper resistance.

Out of scope (interesting, but not "vulnerabilities"):

- Denial-of-service attacks that require an authenticated admin —
  the admin is the trust boundary for administrative actions.
- Attacks that require modifying the DB file directly (bypassing
  the process). If you have arbitrary write to `$DATA_DIR/suchi.db`,
  everything else is moot.
- Attacks on binaries suchi shells out to (qpdf, tesseract,
  ocrmypdf, anydoc) — report those upstream. We'll link the
  upstream advisory in our changelog once patched.
- Attacks that require a third-party plugin not shipped with the
  reference distro.
- Runtime `.so` / dynamic plugin loading — suchi does not support
  runtime plugins; every plugin is compile-linked into a distro
  build via `plugins/index.go`.
- Multi-tenancy inside a single process — suchi's tenancy model is
  instance-per-tenant. Nothing in-process is designed to isolate
  untrusted tenants sharing a process.

## Posture (defense-in-depth)

- **Zero telemetry, ever.** Stock install makes zero outbound
  connections. Every egress is opt-in, listed at boot on
  `main.egress.surface`, and documented on the
  [privacy](docs/privacy.mdx) page.
- **Subprocess sandbox.** Every external binary (qpdf, tesseract,
  ocrmypdf, msgconvert, djvutxt, anydoc, pre-consume scripts) runs
  through `core/sandbox/` with hard timeouts, output caps, empty
  env, and process-group kill on unix.
- **Audit log from Phase 0.** `audit_events` records every mutation
  with (actor, action, object_kind, object_id, before, after,
  request_id, ts). Never contains document content.
- **API tokens hashed at rest.** Plaintext exists only in the
  response that minted the token.
- **Uploads stream to disk.** Never buffered in RAM; body-size caps
  are honored before the CAS put.
- **Parameterized SQL everywhere.** Every string that reaches
  `Exec`/`Query` uses `?` bind parameters. Static-checked by
  `staticcheck` and reviewed by hand.
- **CSP + frame-ancestors + nosniff** on every UI response.
- **CSRF posture**: `SameSite=Lax` on every session cookie plus a
  stateless `Sec-Fetch-Site` middleware that rejects cross-site
  state-changing requests (POST/PATCH/PUT/DELETE) from
  cookie-authenticated callers with 403. Token-authenticated
  requests are exempt — headers can't be forged cross-site.
- **Rate limits** on auth endpoints — 5rps + burst 10 per source IP
  on `POST /api/login`, `POST /setup`, `POST /bootstrap`,
  `POST /api/token/*` (mint + revoke), and the anonymous share-link
  fetch/download paths (`GET /s/{token}` and
  `GET /s/{token}/{doc_id}/download`) which verify the share
  password server-side.
- **ACL layer** (Phase 6). Every doc read/mutate goes through the
  `Authorizer` interface. Search filters by visibility for
  non-admins so snippets don't leak. See
  [permissions](docs/permissions.mdx).

## Deployment expectations

Users are expected to:

- **Keep suchi up to date.** Each MINOR release may include security
  fixes. See [release-process](docs/release-process.mdx).
- **Terminate TLS at a reverse proxy** OR set `TLS_CERT_FILE` +
  `TLS_KEY_FILE`. Plaintext-HTTP-over-the-internet is not a
  supported configuration.
- **Set `PUBLIC_URL`** to the URL clients actually reach — cookies +
  OIDC callbacks + share links all key off it.
- **Protect `$DATA_DIR/.decrypt-key` and `$DATA_DIR/.session-key`.**
  Losing them loses the corresponding data (encrypted-PDF
  passwords; user sessions). Back up `$DATA_DIR` wholesale — see
  [backup-restore](docs/backup-restore.mdx).
- **Restrict access to the docker socket** when
  `MAIL_SETUP_ENV_PATH` is set — the mail-setup wizard uses it to
  restart the sidecar.
- **Do not run as root.** Docker images pre-own `/data` as UID
  65532; bare-binary systemd installs should use an unprivileged
  user (the sample unit in `deploy/systemd/suchi.service` does).

## Regulated data

If you use suchi with PHI, payment-card data, or government IDs, be
aware:

- Suchi is not certified HIPAA / PCI / SOC-2 as-is.
- The audit log is append-only but not tamper-evident (the
  forensic hash-chain tier is Enterprise E3, not shipped).
- Blob-level encryption at rest is planned (E3) but not shipped.
- Retention policies anchored on JD are planned (E3) but not
  shipped.

Deploying suchi under a compliance regime is possible but requires
operator effort — see [comparison](docs/comparison.mdx) for what's
shipped vs planned.

---

Last updated: 2026-08-05.
