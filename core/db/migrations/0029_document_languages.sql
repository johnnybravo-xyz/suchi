-- Per-document language metadata. Populated by the post-ingest
-- language step from (in order) PDF /Lang, email Content-Language,
-- the LLM classifier's response field, or a plugin-registered
-- detector. Empty string means "not detected yet" — the user can
-- fill it in via PATCH.
--
-- Encoding: ISO-639-1 codes, comma-bracketed (e.g. `,de,` or `,de,en,`).
-- The leading and trailing commas make a `LIKE '%,de,%'` filter
-- prefix-safe — `de` won't false-match `deu` or `der`.
--
-- languages_locked = 1 means a human set the value via API/UI and
-- the automatic detector must not overwrite it. Detector steps
-- respect the lock; user PATCH sets it implicitly.

ALTER TABLE documents ADD COLUMN languages        TEXT    NOT NULL DEFAULT '';
ALTER TABLE documents ADD COLUMN languages_locked INTEGER NOT NULL DEFAULT 0;

-- Partial index — the empty default value dominates the table at
-- first, so a full index would be mostly wasted. Filter to rows
-- that carry a value so `?lang=` filters stay cheap once the
-- archive starts accumulating detected languages.
CREATE INDEX documents_languages ON documents(languages) WHERE languages != '';
