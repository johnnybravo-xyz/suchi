-- 0003_fts5: full-text search over documents (title + content).
--
-- We use FTS5 in **external-content** mode with documents as the source.
-- External-content means the FTS table stores only tokens + rowids; the
-- text lives in documents.title and documents.content. Backups stay
-- atomic because everything (source + index) lives in one .db file — the
-- design doc calls this out explicitly.
--
-- Tokenizer: unicode61 (Unicode-aware, folds case + strips diacritics)
-- with the porter stemmer chained on so "invoice" and "invoices" collide.
--
-- Triggers: keep FTS in lockstep with documents. The porter chain forces
-- us to also implement "delete" and "delete-insert" cycles for updates
-- (FTS5 external-content quirk — you rewrite the row, not patch it).
--
-- **JD-prefix search** (`jd:22`, `jd:2*`) is deliberately NOT expressed in
-- FTS. The search handler parses those tokens out of the query string
-- and translates them to a WHERE jd_category_id = ? clause. Baking
-- category codes into the FTS index would tie us to the current tree
-- shape and make renames a reindex.

CREATE VIRTUAL TABLE documents_fts USING fts5 (
    title,
    content,
    content='documents',
    content_rowid='id',
    tokenize='porter unicode61 remove_diacritics 2'
);

-- Keep index synced with the source. Trigger names are prefixed with the
-- table so they show up together in schema dumps.

CREATE TRIGGER documents_fts_ai AFTER INSERT ON documents BEGIN
    INSERT INTO documents_fts(rowid, title, content)
    VALUES (new.id, new.title, coalesce(new.content, ''));
END;

CREATE TRIGGER documents_fts_ad AFTER DELETE ON documents BEGIN
    INSERT INTO documents_fts(documents_fts, rowid, title, content)
    VALUES ('delete', old.id, old.title, coalesce(old.content, ''));
END;

CREATE TRIGGER documents_fts_au AFTER UPDATE OF title, content ON documents BEGIN
    INSERT INTO documents_fts(documents_fts, rowid, title, content)
    VALUES ('delete', old.id, old.title, coalesce(old.content, ''));
    INSERT INTO documents_fts(rowid, title, content)
    VALUES (new.id, new.title, coalesce(new.content, ''));
END;

-- Guard against future ALTER TABLE column-rename surprises: leave this
-- doc-comment migration as the single source of the FTS contract.
