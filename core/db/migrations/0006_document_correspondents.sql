-- 0006_document_correspondents: multi-correspondent junction with role.
--
-- Motivation (widely-requested ecosystem-survey item):
-- real docs have >1 party — a bank statement has {bank, account
-- holder}; an invoice has {vendor, customer}. A single-correspondent
-- model forces the other party into a tag or a custom field, which
-- loses semantic clarity.
--
-- Approach: keep documents.correspondent_id as the *primary* FK (the
-- sender by convention; every existing code path still works). Add a
-- junction that carries any number of additional correspondents with
-- explicit roles.
--
-- Roles are a closed vocabulary today so the UI and rules engine have
-- something stable to key on:
--
--   sender       — issuer of the document (default; matches
--                  documents.correspondent_id semantics)
--   recipient    — the addressee / account holder / customer
--   cc           — copied party (banks CC'ing a co-holder, etc.)
--   other        — everything else with a name attached
--
-- Primary key on (document_id, correspondent_id, role) — a
-- correspondent can appear in multiple roles for the same doc
-- (rare but legal: BESCOM as both sender AND recipient on a
-- correction letter). No same-role duplicates.

CREATE TABLE document_correspondents (
    document_id      INTEGER NOT NULL REFERENCES documents(id) ON DELETE CASCADE,
    correspondent_id INTEGER NOT NULL REFERENCES correspondents(id) ON DELETE CASCADE,
    role             TEXT NOT NULL CHECK (role IN ('sender','recipient','cc','other')),
    position         INTEGER NOT NULL DEFAULT 0,
    PRIMARY KEY (document_id, correspondent_id, role)
) STRICT;

CREATE INDEX document_correspondents_doc  ON document_correspondents(document_id);
CREATE INDEX document_correspondents_corr ON document_correspondents(correspondent_id);

-- Backfill: every existing documents.correspondent_id gets mirrored
-- as a sender-role junction row. Idempotent since the migration only
-- runs once (user_version gate).
INSERT OR IGNORE INTO document_correspondents(document_id, correspondent_id, role, position)
SELECT id, correspondent_id, 'sender', 0
FROM documents
WHERE correspondent_id IS NOT NULL;
