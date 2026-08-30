# Changelog

Notable user-visible changes to Suchi are recorded here.

## [Unreleased]

### Added

- One bounded rich query language across ranked search, Documents, the
  omnibox, and new saved views, with text prefixes, phrases, negation,
  filing metadata, dates, document state, qualifier suggestions, and
  apply-time validation.
- A scoped Archive research desk with reauthorized follow-ups, structured
  citation validation, exact source-set Views, bounded model traffic, and
  explicit sensitive-evidence consent.
- A generic human-reviewed intelligence ledger, with conservative single-call
  date extraction, bulk approval, accepted-date Calendar, and rich-query date
  filters.
- A first-visit public-demo guide for rich queries and the Archive research
  workflow, plus a persistent help launcher. Public model access remains
  denied; the tour uses an anonymized Northstar document cluster.

### Fixed

- Dashboard recent documents load once per navigation instead of retriggering
  from their own response state.
- Calendar resolves an owned or shared saved View by ID on the server, applies
  its complete filter, and then reapplies document ACLs.

## [0.1.0-beta.1] - 2026-08-29

### Added

- A local-first document archive in one Go binary, with an embedded web app,
  SQLite storage, full-text search, metadata, versions, and filing trees.
- Browser, API, watched-folder, and rules-based IMAP intake, including Microsoft
  sign-in, app-password providers, and Paperless-ngx bundle import.
- Ready-made filing trees, deterministic automations, optional local or hosted
  classification, approvals, and permanent Archive configuration.
- Source history, saved and shared views, secure email previews, document
  sharing, API tokens, MCP access, and multi-user permissions.

### Changed

- Fresh installations start with only System/Inbox and require an explicit
  filing-tree or Blank choice; setup controls remain available afterward.
- Account and archive configuration have separate workspaces, with one home for
  mailbox, automation, taxonomy, and saved-view actions.
- Duplicate content records each distinct source, explicit configuration stays
  authoritative, and inactive interface work is deferred for faster navigation.

### Fixed

- Mail intake advances ignored messages correctly, filters only ingestible
  attachments, and no longer leaves attachment-only source emails in Trash.
- Setup reminders retire after engagement, filing-tree selection, completion,
  or 48 hours instead of blocking Archive configuration indefinitely.
- Access checks now cover every document action; Confidential and Restricted
  thumbnails, previews, and extracted text share the same reveal protection.
- Filing-tree replacement, classification, polling, ingest jobs, taxonomy
  import, and configuration reloads preserve state and recover cleanly.
- Saved-view links, missing thumbnails, fast route changes, pipeline rescans,
  and password-protected documents no longer produce misleading interface state.

### Security

- Session and token secrets are digested; provider keys, mailbox credentials,
  and saved document passwords are sealed at rest.
- Setup, demo isolation, error responses, and capability removal fail closed.

[Unreleased]: https://github.com/johnnybravo-xyz/suchi/compare/v0.1.0-beta.1...HEAD
[0.1.0-beta.1]: https://github.com/johnnybravo-xyz/suchi/releases/tag/v0.1.0-beta.1
