# Security policy

## Report a vulnerability

Do not open a public issue for a suspected security problem. Use GitHub's
**Security > Advisories > Report a vulnerability** form for this repository.

Include the affected version, impact, reproduction steps, and any known
mitigation. Sample documents and credentials should contain no real personal
data.

The maintainer will acknowledge the report, reproduce and assess it, then
coordinate a fix and disclosure date with the reporter. Public disclosure
normally follows a patched release. Reporter credit is optional.

## Scope

Reports about the shipped Suchi binary, official images, HTTP and
authentication surfaces, setup flows, storage, authorization, and bundled
plugins are in scope.

Issues requiring direct write access to `DATA_DIR`, a modified build, or an
unbundled third-party plugin are generally outside the project boundary.
Vulnerabilities in external tools such as Tesseract, qpdf, or OCRmyPDF should
also be reported upstream; Suchi will track relevant fixes in its releases.

Pre-1.0 releases support the current published version. Security fixes may
include breaking changes when required to protect data.

## Deployment responsibilities

- Keep Suchi and its external processing tools updated.
- Terminate TLS at a trusted reverse proxy or configure `TLS_CERT_FILE` and
  `TLS_KEY_FILE`.
- Set `PUBLIC_URL` to the address users actually reach.
- Run the process as an unprivileged user and restrict access to `DATA_DIR`.
- Back up all of `DATA_DIR`, including its generated credential-encryption key.
  Losing it makes sealed credentials unusable.
- Set an `https` `PUBLIC_URL` for any TLS deployment. Suchi marks local and
  OIDC browser cookies Secure from that configured URL.

Document preview and download routes enforce the same owner, group, and object
ACL checks as metadata routes. API tokens and browser session identifiers are
stored only as SHA-256 digests. `POST /api/logout` deletes the current browser
session, revokes the current API token when one authenticated the request, and
expires the session cookie.

The first-run setup token is single-use and valid for 24 hours. An expired
attempt mints a replacement and writes it to the server log; setup requests are
serialized so concurrent submissions cannot create multiple administrators.

## Public OAuth identifiers

Suchi's shipped Microsoft application client ID is intentionally committed to
source. A public-client ID identifies the application but does not authenticate
it, and it is necessarily visible in OAuth requests and compiled builds. It
must not be treated as a secret. Like any public-client ID, it can be copied,
so the registration cannot prove that a request came from an official Suchi
binary.

Suchi does not use a Microsoft client secret. Access still requires an
interactive user or tenant consent grant for delegated IMAP access, and the
resulting token cache is sealed in `DATA_DIR`. Report an exposed token, sealing
key, password, or confidential-client credential as a vulnerability; exposure
of the shipped public client ID alone is not one. The project should keep the
delegated scope minimal, verify its publisher, and monitor the registration.
Publisher verification identifies the organization that owns the registration;
it does not stop another program from sending that public client ID. Device-code
sign-in should therefore only be completed after the user starts it in Suchi and
enters the short-lived code shown by that Suchi instance on Microsoft's page.

The built-in egress inventory covers Suchi-managed OIDC, IMAP, Microsoft OAuth,
webhook, and LLM destinations. An operator-configured pre-consume executable
runs with the Suchi process's network access and may contact arbitrary hosts;
restrict host or container egress when that hook must be offline.

See the [configuration](docs/config.mdx),
[backup and restore](docs/backup-restore.mdx), and
[privacy](docs/privacy.mdx) guides for operational details.

## Compliance

Suchi is not certified for HIPAA, PCI DSS, SOC 2, or similar regimes. Operators
handling regulated data must evaluate and provide the controls their regime
requires.
