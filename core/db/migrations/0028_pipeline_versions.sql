-- Per-doc pipeline version columns. Set by the post-ingest handler
-- to the current engine/config signature at the moment of each write.
-- The `suchi rescan` selection default (`--stale <kind>`) targets
-- docs whose signature lags the current binary's — change the OCR
-- engine, bump `pipeline_version_ocr` in code, and only those docs
-- rescan next time. Zero is the "never processed by this pipeline"
-- state a fresh row lands in before any post-ingest tick.
--
-- Kept nullable-with-default-0 (STRICT tables need explicit non-null).
-- Backfill: all existing docs default to 0 and will be picked up by
-- the first `suchi rescan --stale ...` unless the operator scopes
-- narrower.

ALTER TABLE documents ADD COLUMN pipeline_version_ocr     INTEGER NOT NULL DEFAULT 0;
ALTER TABLE documents ADD COLUMN pipeline_version_llm     INTEGER NOT NULL DEFAULT 0;
ALTER TABLE documents ADD COLUMN pipeline_version_content INTEGER NOT NULL DEFAULT 0;
