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

Migration notes get their own **Migration** callouts inside a section
when they need operator action, such as environment-variable renames or
schema compatibility notes.

## [Unreleased]

Everything since the last tag lands here and rolls into the next
version header when a tag is cut.

### Added

- Document details now show every distinct upload, mailbox, watched-folder, or
  import source, when Suchi first saw it, and the source-carried date.

### Changed

- Repeated owner-scoped content now reuses the existing document and records a
  distinct acquisition source without rerunning extraction or classification.
- Office, OpenDocument, RTF, EPUB, and spreadsheet extraction now uses anydoc v0.2.2.
- Local archive matching now runs before user automations and exposes live review and auto-apply thresholds in classification settings.
- The optional model now uses one confidence threshold; fixed classifier plumbing no longer appears as editable automations.
- Mailbox poll intervals are bounded to 1–1440 minutes, source changes reset UID cursors, and disabled owners stop being polled.
- Automation actions are validated when saved, malformed trigger patterns are rejected, and a failed action rolls back the affected automation.
- `PUBLIC_URL` must be a valid HTTP(S) origin; secure-cookie behavior follows its scheme behind reverse proxies.
- Release publishing now requires a SemVer tag, matching changelog entry, and a real Suchi Microsoft client ID.

### Removed

- Removed the unclaimed mobile handshake endpoints and coarse API-token scope aliases.
- Removed JSON/YAML config-file parsing; operator config files are TOML or HuML.
- Local JSON and form login now consistently use `email` instead of a `username` alias.
- Removed legacy single-mailbox environment seeding and the unused session-key-file setting.
- Removed the invalid `remove_owner` automation action; documents always have an owner.

### Fixed

- Taxonomy CLI imports now honor flags after the input filename, apply merge-mode imports with explicit collision remaps, and export the same portable seeds as the admin API.
- Beta and release-candidate images now receive documented moving channel tags instead of leaving prerelease quick-start commands pointed at an unpublished `latest` image.
- Preview and download routes now enforce document ACLs, and new document versions retain the predecessor's grants.
- Bulk trash/delete uses delete permission, multi-tag filtering happens before pagination, and malformed FTS queries return `400` instead of server errors.
- Upload progress now follows the durable ingest job, while scan-split retries fill missing children before retiring the parent.
- Mailbox tests use the saved TLS mode and custom CA, oversized messages no longer block the cursor, and concurrent Outlook deletions do not mark the mailbox unhealthy.
- Existing archive categories are no longer overwritten by local classification; only Inbox or unset categories are eligible.
- Filesystem-watch settings restore the previous values when a live reload fails.
- Activity polling returns the newest visible tail and keeps document, approval, and actor-owned events within their authorization boundary.
- Explicit config-file and environment fields now remain authoritative over stored web settings.
- Invalid empty listener addresses fail during configuration instead of reaching the healthcheck path.

### Security

- Browser session identifiers are stored as SHA-256 digests, disabled users are rejected immediately, and `POST /api/logout` revokes the active session or token.
- First-run setup tokens expire after 24 hours, reject weak passwords, and serialize concurrent submissions.
- Public demo visitors no longer share a known-password administrator; scratch users can mutate only their own documents.
- Internal and upstream failures no longer expose provider, database, filesystem, or cryptographic details in API responses.
- Local login equalizes unknown-account password work and rejects external protocol-relative redirect targets.

---

## Release history

No tags cut yet. `v0.1` is the first line in the sand.
