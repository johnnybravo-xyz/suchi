-- 0014_email_parent.sql
--
-- Email ingest lineage. When a .eml lands via fs-watch (typically
-- from mbsync/isync pulling a Maildir off Proton Bridge or similar),
-- post-ingest parses it and fans out one child document per
-- attachment. The parent doc holds the human-readable subject + body
-- text (searchable via FTS); each child carries the attachment's
-- own bytes and downstream pipeline treatment (OCR, ZUGFeRD, etc.).
--
-- Same shape as split_parent_id: the parent survives (not
-- soft-deleted like the split case, because the email body itself
-- is often the useful document — the .pdf attachment is
-- supplementary). Children point back so the detail UI can render
-- "Attachment of email #N".
--
-- Message-ID captured for future dedup on re-syncs — mbsync will
-- deliver the same .eml file whenever the mbsyncstate is lost, and
-- we want to recognize repeats.

ALTER TABLE documents ADD COLUMN email_parent_id  INTEGER
    REFERENCES documents(id) ON DELETE SET NULL;
ALTER TABLE documents ADD COLUMN email_message_id TEXT;

CREATE INDEX documents_email_parent
    ON documents(email_parent_id)
    WHERE email_parent_id IS NOT NULL;

CREATE INDEX documents_email_message_id
    ON documents(email_message_id)
    WHERE email_message_id IS NOT NULL;
