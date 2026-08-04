-- 0010_document_splits.sql
--
-- Multi-doc splitting on scan intake. When post-ingest detects
-- separator sheets (QR-tagged pages carrying the SCAN_SPLIT_TOKEN),
-- the original upload is broken into N sibling documents. Each child
-- gets its own `original_blob` (a fresh CAS put of the trimmed PDF)
-- and a back-pointer to the parent.
--
-- The parent document itself is soft-deleted after splitting — the
-- CAS blob stays (children reference it via their own put; dedup
-- kicks in if the trimmed bytes happen to match). Undelete on the
-- parent surfaces the original if the split was wrong.
--
-- split_index is 1-indexed and lets the UI show a stable order even
-- if a child gets soft-deleted later ("split 2/4" style).

ALTER TABLE documents ADD COLUMN split_parent_id INTEGER
    REFERENCES documents(id) ON DELETE SET NULL;
ALTER TABLE documents ADD COLUMN split_index INTEGER;

CREATE INDEX documents_split_parent
    ON documents(split_parent_id)
    WHERE split_parent_id IS NOT NULL;
