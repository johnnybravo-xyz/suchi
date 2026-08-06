-- documents.thumb_sha — CAS handle to the 100-DPI page-1 rasterization
-- generated at ingest time via pdftoppm. Populated only for docs with
-- an archive PDF; non-PDF ingest paths leave it NULL and the endpoint
-- 404s so the SPA falls back to its initials-style placeholder.
--
-- No index — reads are always by documents.id, not by thumb_sha.

ALTER TABLE documents ADD COLUMN thumb_sha TEXT;
