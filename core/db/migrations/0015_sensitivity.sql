-- documents.sensitivity — an optional classification label. Kept as
-- free-text (not an enum) so future value additions don't need a
-- migration. Common conventions to adopt without hard-coding:
--   'confidential' | 'internal' | 'public'
-- Left NULL for anything unclassified. Read paths and DLP checks
-- gate on presence; the column itself is inert until something
-- downstream reads it.
ALTER TABLE documents ADD COLUMN sensitivity TEXT;
