-- 0005_email_accounts_sync_since.sql
--
-- Per-account initial-sync horizon. Without this, a freshly-added
-- account pulls every UNSEEN message from INBOX regardless of age —
-- verified against ritesh_shrv@live.com where 4+ old Microsoft
-- security notifications landed as docs seconds after connect.
--
-- Unix timestamp; NULL preserves the pre-change "sync all UNSEEN"
-- behaviour so existing rows are untouched on upgrade. The API layer
-- defaults new accounts to time.Now().Unix() at insert so the SPA
-- "add mailbox" flow only pulls fresh mail. Watcher maps the value
-- onto imap.SearchCriteria.Since (server-side INTERNALDATE filter).

ALTER TABLE email_accounts ADD COLUMN sync_since INTEGER;
