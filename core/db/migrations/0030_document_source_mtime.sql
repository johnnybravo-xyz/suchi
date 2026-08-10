-- Source-file mtime carried through ingest. This is the filesystem
-- "last modified" of the file the operator uploaded, imported, or
-- dropped into a watched directory — the closest thing to a real
-- creation date the archive has. `added_at` is when the row was
-- created in this DB; `source_mtime` is when the underlying document
-- came into being (or was last touched) before it landed here.
--
-- NULL means the ingest path didn't (or couldn't) capture it — e.g.
-- a browser upload where the SPA didn't send `lastModified`, or a
-- pre-migration row. The document detail view falls back to
-- `added_at` in that case.
--
-- Unix seconds. No partial index — most rows will populate this
-- once new upload paths land, and NULLs sort last in ORDER BY.

ALTER TABLE documents ADD COLUMN source_mtime INTEGER;
