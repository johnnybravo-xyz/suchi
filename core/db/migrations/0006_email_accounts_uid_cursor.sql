-- 0006_email_accounts_uid_cursor.sql
--
-- Server-side UID cursor for the poll loop, plus an opt-in mark_seen
-- toggle that preserves the pre-cursor behaviour.
--
-- Before this migration the watcher searched `WithoutFlags \Seen` and
-- STORE'd +\Seen after every ingest. That kept the search idempotent
-- but hijacked the operator's own read/unread signal in the mail
-- client — a real friction point once the archive shares a mailbox
-- with human triage.
--
-- After: the cursor (last_uid_seen) is the idempotency source. Search
-- criteria becomes `UID <cursor+1>:*  AND SINCE <sync_since>`. Nothing
-- STOREs \Seen unless the operator explicitly asked for it via
-- mark_seen, and processed-folder moves are unchanged (a separate
-- "archive after ingest" convention).
--
-- uidvalidity_seen tracks the last-observed UIDVALIDITY of the folder.
-- If UIDVALIDITY changes (folder recreated, mailbox reset), UIDs
-- restart from 1 and our cursor is meaningless — the watcher resets
-- last_uid_seen to 0 and logs the reset. Standard IMAP client hygiene.
--
-- Defaults keep existing rows quietly correct on upgrade:
--   last_uid_seen = 0        → first cycle picks up from oldest UID
--                              matching SINCE (same effective floor
--                              as the previous UNSEEN behaviour)
--   uidvalidity_seen = 0     → first Select stamps the current value
--   mark_seen = 0            → new default matches "operator wants to
--                              own the read state"; operators who
--                              relied on the old behaviour flip it on
--                              via the SPA (see MailAccountForm.svelte).

ALTER TABLE email_accounts ADD COLUMN last_uid_seen INTEGER NOT NULL DEFAULT 0;
ALTER TABLE email_accounts ADD COLUMN uidvalidity_seen INTEGER NOT NULL DEFAULT 0;
ALTER TABLE email_accounts ADD COLUMN mark_seen INTEGER NOT NULL DEFAULT 0;
