-- 0008_decryption_passwords_last_doc.sql
--
-- Track which document a vault password last unlocked, so the
-- Settings vault UI can surface a clickable filename per entry.
-- Nullable; no FK enforcement (SQLite ALTER TABLE limitation and
-- we want deletion of the target document to leave the vault row
-- intact — the UI degrades to "no longer available").

ALTER TABLE decryption_passwords
  ADD COLUMN last_used_doc_id INTEGER;
