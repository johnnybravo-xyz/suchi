ALTER TABLE documents
    ADD COLUMN content_source TEXT NOT NULL DEFAULT ''
    CHECK (content_source IN ('', 'device_ocr', 'server'));

ALTER TABLE documents
    ADD COLUMN device_content_confidence REAL
    CHECK (device_content_confidence BETWEEN 0 AND 1);

ALTER TABLE documents
    ADD COLUMN device_ocr_language TEXT NOT NULL DEFAULT '';

ALTER TABLE documents
    ADD COLUMN device_content_received_at INTEGER;

ALTER TABLE documents
    ADD COLUMN split_origin_id INTEGER NOT NULL DEFAULT 0
    CHECK (split_origin_id >= 0);

CREATE UNIQUE INDEX documents_split_origin_part
    ON documents(split_origin_id, split_index)
    WHERE split_origin_id != 0;

CREATE TABLE upload_idempotency (
    user_id             INTEGER NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    idempotency_key     TEXT    NOT NULL,
    operation           TEXT    NOT NULL CHECK (operation IN ('document', 'version')),
    predecessor_id      INTEGER NOT NULL DEFAULT 0,
    request_fingerprint TEXT    NOT NULL,
    sha256              TEXT    NOT NULL,
    document_id         INTEGER NOT NULL REFERENCES documents(id) ON DELETE CASCADE,
    response_status     INTEGER NOT NULL,
    response_json       TEXT    NOT NULL,
    created_at          INTEGER NOT NULL,
    PRIMARY KEY (user_id, idempotency_key)
) STRICT;
