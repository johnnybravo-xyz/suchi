# Security Policy

**Reporting**: while the repo is private (pre-Phase-4), report privately to
the maintainer via email. Once public, this file will list a security
contact and any advisories.

## Scope

- The suchi binary and every reference plugin in this repo.
- The documented plugin API (`plugin-api/`) — a plugin author writing to
  the interface should not be able to bypass suchi's audit or auth by
  construction.

## Posture (recap from the design doc)

- Zero telemetry, ever. Stock install makes zero outbound connections.
- Every egress is opt-in, visible in config, and logged (grep
  `main.egress.surface` in the boot log).
- Subprocesses that parse user bytes run with hard timeouts, rlimits,
  output caps, and no network (arrives in Phase 2 with the OCR plugin).
- Audit log from Phase 0. `audit_events` never contains document content.
- API tokens are hashed at rest; the plaintext exists only in the response
  that created it.
- Uploads stream to disk, never buffered in RAM (Phase 2).

## Not-in-scope (until noted)

- suchi is pre-alpha. Do not run it in production.
- Runtime `.so` / third-party dynamic plugins are not supported — every
  plugin is compile-linked into a distro build (see `plugin-api/`).
- Multi-tenancy is instance-per-tenant; nothing in-process is designed
  to isolate untrusted tenants sharing a process.
