-- 0011_decryption.sql
--
-- Password-protected file support. Encrypted PDFs (and, in later work,
-- encrypted office docs / archives) land in state='encrypted' with
-- their content extraction pipeline halted; the operator supplies a
-- password via /api/documents/{id}/decrypt (or the pending-decryption
-- UI), which runs qpdf against the encrypted CAS blob, stores the
-- decrypted output as a second CAS blob, and re-enqueues post-ingest.
--
-- Original bytes are ALWAYS preserved verbatim in original_blob. The
-- decrypted working copy lives in decrypted_blob so the audit trail
-- ("here's exactly what the user uploaded") is intact regardless of
-- how many times we re-decrypt.
--
-- encryption_state vocabulary is deliberately narrow:
--   NULL        — not encrypted, or never checked (pre-feature docs)
--   'encrypted' — decrypt attempts failed; awaiting password
--   'decrypted' — was encrypted, password supplied, working copy live

ALTER TABLE documents ADD COLUMN encryption_state TEXT
    CHECK (encryption_state IN ('encrypted', 'decrypted'));
ALTER TABLE documents ADD COLUMN decrypted_blob TEXT;
ALTER TABLE documents ADD COLUMN decrypted_size INTEGER;

CREATE INDEX documents_encryption_state
    ON documents(encryption_state)
    WHERE encryption_state = 'encrypted';

-- Learned passwords. When a user unlocks a doc with `remember=true`,
-- the password lands here — AEAD-sealed with the .decrypt-key file so
-- a DB-only compromise can't recover cleartext. Owner-scoped so
-- household members' passwords don't cross-contaminate.
--
-- On subsequent encrypted uploads, post-ingest tries every stored
-- password for the doc's owner (ordered by last_used_at DESC so hot
-- passwords hit first). A successful attempt bumps last_used_at.
CREATE TABLE decryption_passwords (
    id            INTEGER PRIMARY KEY,
    owner_id      INTEGER NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    ciphertext    BLOB NOT NULL,   -- AES-256-GCM seal of the password bytes
    label         TEXT,             -- optional operator-visible name ("BofA 2024")
    created_at    INTEGER NOT NULL,
    last_used_at  INTEGER
) STRICT;

CREATE INDEX decryption_passwords_owner
    ON decryption_passwords(owner_id, last_used_at DESC);
