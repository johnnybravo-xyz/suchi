-- v0.1.0-beta.2 schema changes. Immutable after publication.
-- Existing tag assignments have no provenance and remain user-controlled.
ALTER TABLE document_tags ADD COLUMN classifier_owned INTEGER NOT NULL DEFAULT 0
    CHECK (classifier_owned IN (0, 1));

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

-- Stream the default live-document page without a temporary sort.
CREATE INDEX documents_live_created
    ON documents(created_at DESC, id DESC)
    WHERE trashed_at IS NULL;

-- Repair preset keyword matching. Preset edits become user-owned copies;
-- only generated, still-preset-owned keyword rules receive word boundaries.
UPDATE automation_triggers
SET filter_content_re = '(?:^|[^\p{L}\p{N}_])(?:' || filter_content_re || ')(?:$|[^\p{L}\p{N}_])'
WHERE type = 'document_added'
  AND filter_content_re IS NOT NULL AND filter_content_re != ''
  AND instr(filter_content_re, '(?:^|[^\p{L}\p{N}_])(?:') != 1
  AND automation_id IN (
    SELECT a.id FROM automations a
    JOIN automation_actions aa ON aa.automation_id = a.id
    WHERE COALESCE(a.preset_slug, '') != ''
      AND aa.kind = 'assign_jd_category'
      AND json_type(aa.params_json, '$._preset_keywords') = 'array'
      AND json_array_length(aa.params_json, '$._preset_keywords') > 0
  );
