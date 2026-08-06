-- document_proposals — the queue for auto-file-from-archive suggestions
-- that the operator resolves from the Tasks inbox. One row per (doc,
-- field-or-tag) proposal, produced by the apply_from_similar automation
-- action when confidence falls in the propose tier (>= threshold_propose
-- but < threshold_autoapply). Rows with confidence at the autoapply tier
-- are written directly to the doc and never land here.
--
-- ON DELETE CASCADE so a soft-then-hard doc delete drops its pending
-- proposals with it. The partial index keeps the Tasks-feed lookup cheap
-- (only pending rows matter to the inbox aggregator).

CREATE TABLE document_proposals (
    id           INTEGER PRIMARY KEY,
    document_id  INTEGER NOT NULL REFERENCES documents(id) ON DELETE CASCADE,
    field        TEXT NOT NULL,               -- 'jd_category' | 'correspondent' | 'document_type' | 'tag'
    value_id     INTEGER,                     -- the proposed id (tag_id / correspondent_id / …); nullable to leave room for typed-value proposals
    value_json   TEXT NOT NULL,               -- {id, label, supporters:[docIDs]} — cached for the SPA card so it doesn't re-hit the taxonomy tables
    confidence   REAL NOT NULL,               -- 0.0..1.0
    based_on     TEXT NOT NULL,               -- JSON array of neighbour doc IDs that drove this proposal
    created_at   INTEGER NOT NULL,
    resolved_at  INTEGER,                     -- NULL = pending
    resolution   TEXT                         -- 'applied' | 'rejected'
) STRICT;

CREATE INDEX document_proposals_pending
    ON document_proposals(document_id) WHERE resolved_at IS NULL;
