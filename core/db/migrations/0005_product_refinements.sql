-- SPDX-License-Identifier: AGPL-3.0-or-later

CREATE TABLE local_auth_totp (
    user_id           INTEGER PRIMARY KEY REFERENCES users(id) ON DELETE CASCADE,
    secret_ciphertext BLOB NOT NULL,
    last_used_step    INTEGER NOT NULL CHECK (last_used_step >= 0),
    failed_attempts   INTEGER NOT NULL DEFAULT 0 CHECK (failed_attempts >= 0),
    blocked_until     INTEGER NOT NULL DEFAULT 0,
    enabled_at        INTEGER NOT NULL
) STRICT;

CREATE TABLE local_auth_recovery_codes (
    user_id     INTEGER NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    code_digest BLOB NOT NULL,
    created_at  INTEGER NOT NULL,
    PRIMARY KEY (user_id, code_digest)
) STRICT;

-- Email attachment children inherit the message's sources, whose detail names
-- the mailbox folder. filename keeps the attachment's own name; other kinds
-- carry the acquired file name in detail and leave this empty.
ALTER TABLE document_sources ADD COLUMN filename TEXT NOT NULL DEFAULT '';

-- client names the official app behind an API upload, set from server-issued
-- token provenance. label stays the uploading person.
ALTER TABLE document_sources ADD COLUMN client TEXT NOT NULL DEFAULT '' CHECK (client IN ('', 'companion_app'));

-- The word and trigram indexes skip writes that leave the indexed title and
-- content unchanged. The revision triggers still count every write, because a
-- same-value write intentionally invalidates evidence.
DROP TRIGGER documents_fts_au;
CREATE TRIGGER documents_fts_au AFTER UPDATE OF title, content ON documents
WHEN old.title IS NOT new.title OR coalesce(old.content, '') IS NOT coalesce(new.content, '')
BEGIN
    INSERT INTO documents_fts(documents_fts, rowid, title, content)
    VALUES ('delete', old.id, old.title, coalesce(old.content, ''));
    INSERT INTO documents_fts(rowid, title, content)
    VALUES (new.id, new.title, coalesce(new.content, ''));
END;

DROP TRIGGER documents_trigram_au;
CREATE TRIGGER documents_trigram_au AFTER UPDATE OF title, content ON documents
WHEN old.title IS NOT new.title OR coalesce(old.content, '') IS NOT coalesce(new.content, '')
BEGIN
    INSERT INTO documents_trigram(documents_trigram, rowid, title, content)
    VALUES ('delete', old.id, old.title, coalesce(old.content, ''));
    INSERT INTO documents_trigram(rowid, title, content)
    VALUES (new.id, new.title, coalesce(new.content, ''));
END;
