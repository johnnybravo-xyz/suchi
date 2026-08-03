# suchi

**suchi** (Sanskrit *सूची*, "an index, a catalog, a list") — a document-management system as a single Go binary. SQLite by default, content-addressed storage, plugin seams at every layer, and wire-compatible with the Paperless-ngx mobile ecosystem.

Status: **pre-alpha** — Phase 0 in progress. Not for production use. Repo is private until Phase 4.

## Non-negotiables

- One binary, one config, one data dir. No Redis, no Postgres for MVP.
- Idle RAM budget: ~100MB. Idle CPU: near-zero.
- **No telemetry, ever.** A stock install makes zero outbound connections. Every egress is opt-in, visible in `config.yaml`, and logged.
- AGPL-3.0. DCO/CLA once the repo goes public.

See `notes/suchi-plan.md` (upstream design doc) for the full picture.

## Layout

```
suchi/
├── go.work                — workspace linking all modules
├── plugin-api/            — interfaces + shared types; the ONLY dep every module shares
├── core/                  — HTTP, DB, jobs, audit, auth chain. Imports plugin-api only.
├── plugins/               — reference plugins, each its own module
│   ├── local-auth/
│   └── oidc/
└── distro/                — the shipped binary. Pins versions, blank-imports enabled plugins.
    └── cmd/suchi/         — main entry point
```
