# Changelog

Every user-visible change lands here. Format follows
[Keep a Changelog](https://keepachangelog.com/en/1.1.0/); versions
follow [SemVer](https://semver.org/spec/v2.0.0.html).

Section conventions:

- **Added** — new features, endpoints, subcommands, config knobs.
- **Changed** — behavior changes that a user or integrator would
  notice (URL renames, default flips, response-shape edits).
- **Removed** — features / endpoints / knobs gone.
- **Fixed** — bug fixes.
- **Security** — vulnerabilities patched, defense-in-depth
  tightenings. Always call these out separately.

Migration notes get their own **⚠ Migration** callouts inside a
section when they need operator action (env-var renames, schema
changes that require a restart, etc.).

## [Unreleased]

Everything since the last tag. When we cut a release, this section's
contents move under the new version header and a fresh `[Unreleased]`
opens.

### Added

- Refile primitive (`suchi refile` / `POST /api/admin/refile`) —
  bulk re-run classifier + re-render every live doc after a
  preset / template / rule change. Selling point: "you can always
  come back to change this."
- Phase 6 permissions — `groups`, `group_members`, `object_acls`
  behind the `Authorizer` interface. Document-level enforcement
  (view/change/delete bits), search-result filtering, group-based
  grants, admin UI at `/admin/groups`, REST at `/api/groups/*` +
  `/api/acls/{kind}/{id}`.
- Automations engine — trigger→conditions→actions rules
  (`document_added` / `document_updated` / `consumption`),
  `/api/automations/*` CRUD, admin UI at `/admin/automations`.
- Deploy templates under `deploy/` — systemd, Caddy, nginx,
  Traefik, k8s.
- `suchi demo` — seed a fresh DATA_DIR with sample docs, tags,
  one automation, one rule for kick-tires evaluation.
- MCP v2 adapter — `suchi mcp [--http addr]` and `suchi-mcp`
  symlink. Tools: `search_documents`, `get_document`, `list_inbox`,
  `resolve_approval_task`, `create_share_link`.
- Config-file loader — TOML (default), HUML, YAML, JSON. Env wins on
  collisions. `SUCHI_CONFIG` env for explicit override.
- Sensitivity classification — `documents.sensitivity`, blur
  preview for confidential/restricted, sensitivity picker in
  detail UI.
- Search filters — `?tags__id__in`, `?correspondents__id__in`,
  `?document_type__id`, `?jd_category_id`, `?sensitivity` on
  `/api/search/`.
- OpenAPI 3.1 spec at `/api/schema/`.
- DRF pagination envelope on every list endpoint.
- Full mobile-compat surface — `/api/token/`, `/api/remote_version/`,
  `/api/next_asn/`, `/api/saved_views/`, `/api/ui_settings/`,
  `/api/trash/`, `/api/share_links/` + public `/s/{token}`.
- Docs: [`permissions`](docs/permissions.mdx),
  [`automations`](docs/automations.mdx),
  [`approvals`](docs/approvals.mdx) (was workflows),
  [`refile`](docs/refile.mdx), [`backup-restore`](docs/backup-restore.mdx),
  [`comparison`](docs/comparison.mdx).

### Changed

- **URL rename** — the state-machine engine moved from
  `/api/workflows/*` to `/api/approvals/*`. The new
  trigger→conditions→actions engine lives at `/api/automations/*`.
  This keeps the internal naming ("approvals" = human sign-off;
  "automations" = machine rules) matching the URLs.
- MCP tool `resolve_workflow_task` → `resolve_approval_task`
  (same reasoning).
- Column rename `documents.bundle_id_legacy` → `documents.legacy_id`
  (pre-alpha, amended in 0002 without a migration bump).

⚠ **Migration** — nothing here is deployed yet, so no operator
action is needed. If you're running a checkout: update any scripts
that hit `/api/workflows/*` to use `/api/approvals/*`; update MCP
tool calls from `resolve_workflow_task` to `resolve_approval_task`.

### Security

- Search results now filter by ACL visibility — snippets no longer
  leak content of docs the caller can't view.
- Document sub-endpoints (correspondents, versions, custom-field
  values) enforce ACL grants via the shared `Authorizer` interface,
  replacing scattered `owner_id`-scoped queries.

---

## Release history

Empty until the first tag lands. The `[Unreleased]` block above
captures everything shipped so far (pre-alpha, no versions cut).
