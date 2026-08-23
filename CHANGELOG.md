# Changelog

Notable user-visible changes to Suchi are recorded here.

## [Unreleased]

## [0.1.0-beta.1] - 2026-08-23

### Added

- A local-first document archive in one Go binary with an embedded responsive
  web UI, SQLite storage, content-addressed files, full-text search, metadata,
  saved views, document versions, access controls, and Johnny.Decimal filing
  trees.
- Intent-led setup with ready-made filing trees, watched folders, IMAP mailbox
  intake, browser and API uploads, and Paperless-ngx bundle imports.
- Guided Microsoft sign-in plus app-password setup for Gmail, iCloud, and other
  IMAP providers.
- Composable mailbox intake rules with per-rule message matching, attachment
  handling, and a bounded live preview.
- Rules-first classification with optional local Ollama or hosted
  OpenAI-compatible classification, confidence controls, and human review.
- Source history for repeated uploads, mailbox messages, watched files, and
  imports, including the source-carried date.
- Sandboxed inline previews for archived email bodies, with remote content
  blocked to avoid tracking requests.
- Deterministic automations, document sharing, API tokens, MCP access, and
  audit-backed approvals.

### Changed

- Duplicate content owned by the same user reuses its document while recording
  every distinct source.
- Configuration from an explicit file or environment remains authoritative over
  stored web settings.
- Fresh installations start with a neutral System/Inbox baseline. Setup requires
  an explicit filing-tree or Blank choice before it can be completed.
- The activity drawer is hidden until notifications have a clear user-facing
  purpose; approvals and failed jobs remain available on their dedicated page.

### Fixed

- Ignored mailbox messages advance the intake cursor without being moved or
  marked read, and attachment filtering matches the files Suchi can ingest.
- Attachment-only mailbox intake removes temporary source messages after
  fanout instead of filling Trash with system-created email rows.
- Document previews, downloads, sharing, versions, bulk actions, and activity
  feeds consistently enforce access controls.
- Classification preserves explicit categories and correspondents, validates
  provider output, and reports uncertain suggestions for review.
- Setup, mailbox polling, watched folders, durable ingest jobs, taxonomy import,
  and configuration reloads recover cleanly from invalid or partial work.
- Filing-tree replacement preserves live and trashed documents, including when
  existing documents are queued for refiling.
- Fresh incomplete installations show admins a dismissible setup reminder in
  the sidenav, with a mobile dashboard fallback, for 48 hours. Setup remains
  available from Settings, while completion hides the reminder immediately.
- Password-protected PDFs waiting for decryption no longer generate recurring
  pipeline-rescan approvals that cannot advance them.
- Pipeline-rescan approval details identify the affected documents with links
  before an operator approves the work.

### Security

- Session identifiers and API tokens are stored as digests; provider keys,
  mailbox credentials, and saved document passwords are sealed at rest.
- First-run setup expires, rejects weak passwords, and serializes concurrent
  submissions.
- Public demo users are isolated, and API errors avoid exposing internal,
  provider, filesystem, database, or cryptographic details.

[Unreleased]: https://github.com/johnnybravo-xyz/suchi/compare/v0.1.0-beta.1...HEAD
[0.1.0-beta.1]: https://github.com/johnnybravo-xyz/suchi/releases/tag/v0.1.0-beta.1
