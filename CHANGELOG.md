# Changelog

Notable user-visible changes to Suchi are recorded here.

## [Unreleased]

### Added

- One bounded rich query language across ranked search, Documents, the
  omnibox, and new saved views, with text prefixes, phrases, negation,
  filing metadata, dates, document state, qualifier suggestions, and
  apply-time validation.
- A scoped Archive research desk with reauthorized follow-ups, structured
  citation validation, Views made from retrieved documents, bounded model
  traffic, and explicit sensitive-evidence consent.
- A generic extracted-fact ledger with single-call date extraction,
  user-controlled confidence-based Calendar entry, optional bulk review, and
  rich-query date filters.
- A first-visit public-demo guide for rich queries and the Archive research
  workflow, plus a persistent help launcher. Public model access remains
  denied; the tour uses an anonymized Northstar document cluster.
- A fixed 30-day Trash recovery window with automatic expiry, confirmed
  permanent deletion and Empty Trash actions, share-link revocation, minimal
  purge auditing, and reference-safe blob cleanup.

### Changed

- Beta.2 schema changes now ship as one migration, so beta.1 archives advance
  in a single transactional step.
- Rich-query lists now start from matching FTS rows, ranked Search bounds
  recency snippet work to its result page, and newest document pages use a
  stable partial index.
- Bulk document authorization is batched, and concurrent date reviewers now
  report and audit only the decision that actually changed each candidate.
- The web app cancels superseded list, search, Calendar, and completion reads;
  caches Calendar date formatters; loads route CSS lazily; and retains at most
  20 Archive research turns.
- Sampled rescans keep memory proportional to the requested sample, idle
  research rate entries expire, and date extraction holds SQLite's writer for
  less time.

### Fixed

- Approvals bound to trashed documents now disappear from REST and MCP inboxes;
  restoring the document makes them available again only if the task and its
  run are still active.
- Calendar collapses same-document same-day roles into one month-grid card,
  keeps origin badges inside compact cards, and mobile document rows prioritize
  titles over tags.
- Dashboard recent documents load once per navigation instead of retriggering
  from their own response state.
- Calendar resolves an owned or shared saved View by ID on the server, applies
  its complete filter, and then reapplies document ACLs.
- Archive research requests provider JSON mode, normalizes structured citations
  into clickable answer markers, tolerates numeric citation strings, includes
  receipt totals in bounded evidence passages, and logs only the mode, passage
  and source counts, and bounded evidence size—never question or document text.
- Date review uses a responsive document grid, keeps the selected-date actions
  visible, explains the decision, and renders section labels in sentence case.
- Title-only ranked matches return an empty body snippet instead of failing,
  Calendar identifies months that exceed its 500-row display limit, and
  oversized facet lists fail before reaching SQLite.
- HTTP request metrics now record the matched route pattern, including
  normalized API paths, without using document IDs as labels.

### Security

- Model-provider logs retain only the endpoint host, never URL paths or query
  strings that may contain tenant or credential material.

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
