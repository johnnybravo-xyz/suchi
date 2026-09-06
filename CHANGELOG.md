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
  purge auditing. Original and derived blobs remain until offline GC; online
  cleanup cannot safely identify in-flight uploads reusing those bytes.

### Changed

- The web app imports screens directly and defers archive/mailbox configuration,
  removing six route wrappers. At the same dependency pins, Documents loads 48%
  less route JavaScript, Upload 83% less, and My account without mailbox access
  86% less. Finer chunks increase the all-routes compressed total by 8.3%.
- `doctor` drops a misleading shard-capacity sample whose threshold cannot
  occur in the hash-prefix layout. Explicit CAS integrity checks are unchanged.
- Reviewed tool and dependency pins advance to compatible previous-stable
  releases, including Go 1.27.0, Bun 1.4.1, AnyDoc 0.2.3, and SQLite 1.57.
  CI actions use immutable commits, and AnyDoc builds honor its Cargo lockfile.
- Release preflight now includes fresh code/UI checks and advisory scans.
  Artifacts-only branch builds use legal snapshot names and skip signing;
  image publication waits for the binary builds.
- Ready jobs and follow-up pipeline stages drain without a polling delay
  between batches. Document lists load correspondent names in one query.
- The web app reuses its date formatter and removes redundant search state
  updates and request wrappers.
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

- Difficult photographed QR codes now get a bounded local ZBar fallback when
  the Go reader cannot decode them; both runtime images include the tool.
- Lightweight OCR retries empty pages once using sparse-text segmentation for
  isolated labels. Rasterization, pages and retries share one real timeout, and
  cancellation/output-limit failures cannot be recorded as successful empty OCR.
  OCR revision 2 offers the existing controlled rescan path for older results.
- Camera photos retain their EXIF orientation in generated PDFs. Raster images
  use 300-DPI page density without changing pixel resolution, preventing the
  OCR stage from enlarging ordinary phone photos into hundreds of megapixels.
  Content remains at revision 2; selected rescans repair older results from
  immutable originals. Rescan prompts omit internal revision numbers.
- Approval cards omit internal workflow names, assignee IDs and step metadata.
  Decisions retain confidence, evidence and deadlines; rescan proposals list
  their affected documents under an explicit label.
- Created share links remain visible and selectable when clipboard access is
  unavailable. Copy can be retried without creating another link; delayed
  creation responses cannot copy links after account or document navigation.
- Startup rejects duplicate or invalid migration versions before changing any
  schema, identifying the conflicting migrations for developers.
- Dropping files onto the upload dialog submits each file once. Uploads dropped
  elsewhere refresh the visible document list immediately.
- Trash documents open in the existing viewer with read-only metadata and
  owner/admin preview, download, restore, and confirmed permanent deletion.
  Mobile rows keep document titles above their actions; sensitive reveal gates
  and public-share exclusions remain in place.
- Archive research opens its retrieved documents' Calendar dates in an all-dates
  agenda across months and years, with explicit scope and pagination. New date
  links clear stale Calendar filters; the ordinary Calendar stays month-based.
  Partial dates no longer display invented days in Calendar or Approvals;
  Calendar keeps labeled model confidence behind Automatic/Reviewed details.
- Calendar day links open paginated exact-day agendas, retain the month, View,
  and role in the URL, and return to the originating month. Server-side precision
  filtering excludes month/year placeholders before counting or pagination.
- Archive research allows a bounded 4,096-token generation budget so reasoning
  models can finish their JSON answer. Truncation has a distinct error; citation
  validation and the 6,000-character answer limit remain unchanged. Failure logs
  include fixed diagnostic reasons without question, evidence, or provider text.
- Settings identifies the running server version and source revision for bug
  reports. JSON API errors retain their codes even with mislabeled response
  headers, so Archive research can show the relevant failure message.
- Mail attachments with invalid or generic MIME headers are identified from
  their bytes. Explicit rescans repair existing PDFs labeled as binary files
  and restore preview and password handling without changing originals.
- Both container variants now produce real PDFs from images, including HEIC:
  standard includes the missing ImageMagick PDF encoder, and full permits PDF
  writing while keeping decoding restricted. The duplicate HEIC conversion
  path is gone; non-PDF converter output is rejected. Explicitly rescan affected
  images to repair older results; pipeline revisions and originals stay intact.
- Mail smoke checks wait for ingestion, verify OCR text and parse the downloaded
  HEIC archive instead of accepting any non-null blob or a fragile log match.
- Demo visitor expiry also leaves CAS bytes for offline reclamation, preventing
  it from removing an in-flight upload or another document version's original.
  The existing stopped demo-volume reset remains the disk-reclamation path.
- Setup, egress, and filing-recovery logs preserve their structured event name
  instead of emitting duplicate JSON `msg` fields.
- Demo dashboard counts now include the visible corpus. Visitor uploads preview
  and download through the same bounded session after navigation or reload.
- Destructive confirmation dialogs use native modal focus, Escape handling,
  and an inert background, restoring focus when cancelled.
- Permanent deletion no longer removes an in-flight upload's original bytes.
  GC now explicitly requires stopped archive writers, and the restore drill
  verifies a real document's bytes, extracted content, and search after restart.
- Clearing Search cancels its pending request and resets loading/error state.
- Replaying a demo manifest skips existing documents without adding tags or
  jobs to an unrelated document.
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

- MCP refuses redirects, malformed or oversized responses, and unexpected
  content types. Tool errors omit document queries, response bodies and raw
  transport diagnostics.
- Browser demo credentials no longer live in JavaScript storage. Scratch
  sessions retain their restricted identity, expire at the visitor TTL, and
  enforce same-origin mutation checks; concurrent writes share one upgrade.
- Both OIDC sign-in paths now require signed verified-email claims before
  binding local accounts. Providers without truthful `email_verified: true`
  claims are no longer supported. Suchi Bearer tokens work with OIDC enabled,
  and unknown credentials cannot fall back to a browser session.
- Share-link bearers are no longer copied into creation audit events or HTTP
  access logs. Logs and metrics retain matched routes across authentication.
- Signing out clears retained archive data; delayed reads, profile refreshes,
  demo upgrades, and queued uploads cannot carry work into another account.
- Mail and AnyDoc smoke checks now own isolated temporary data and containers,
  bind loopback, and retain scratch data if teardown fails.
- API tokens now enforce a fail-closed route policy. Narrow document/event
  tokens cannot inherit account/settings administration from an admin owner;
  profiling and raw-original downloads require a session. Token management is
  session-only, removing token delegation and cross-user credential revocation
  through narrow admin tokens. Admin-token metrics remain supported.
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
