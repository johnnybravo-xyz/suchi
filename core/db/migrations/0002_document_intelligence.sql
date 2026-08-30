CREATE TABLE document_intelligence (
    id                 INTEGER PRIMARY KEY,
    document_id        INTEGER NOT NULL REFERENCES documents(id) ON DELETE CASCADE,
    intelligence_type  TEXT NOT NULL,
    role               TEXT NOT NULL DEFAULT '',
    value_json         TEXT NOT NULL,
    sort_value         TEXT NOT NULL DEFAULT '',
    raw_text           TEXT NOT NULL DEFAULT '',
    evidence_text      TEXT NOT NULL,
    evidence_start     INTEGER,
    confidence         REAL NOT NULL CHECK (confidence >= 0.0 AND confidence <= 1.0),
    status             TEXT NOT NULL DEFAULT 'pending' CHECK (status IN ('pending','accepted','rejected')),
    extractor          TEXT NOT NULL,
    source_blob        TEXT NOT NULL DEFAULT '',
    extraction_version INTEGER NOT NULL,
    reviewed_by        INTEGER REFERENCES users(id) ON DELETE SET NULL,
    reviewed_at        INTEGER,
    created_at         INTEGER NOT NULL,
    updated_at         INTEGER NOT NULL,
    UNIQUE (document_id, intelligence_type, role, value_json, evidence_text, extractor)
) STRICT;

CREATE INDEX idx_document_intelligence_review
    ON document_intelligence(status, intelligence_type, sort_value, document_id);
CREATE INDEX idx_document_intelligence_document
    ON document_intelligence(document_id, status, intelligence_type);
