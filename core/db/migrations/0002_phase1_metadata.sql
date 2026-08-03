-- 0002_phase1_metadata: Paperless-shape metadata tables + document
-- columns the importer needs. FTS5 arrives in a separate migration
-- (0003) so a schema-only diff on this one stays readable.
--
-- Design contract: table shapes match Paperless-ngx well enough that
-- `suchi import paperless` maps columns verbatim (see design doc §Migrating
-- from Paperless-ngx). Names follow suchi conventions (snake_case, no
-- Django-y `_id_id`), never Paperless internals.

-- ---------- reference tables ----------

-- Tags. matching_algorithm/match/is_insensitive are Paperless workflow
-- inputs; we import them verbatim and let the rules engine (Phase 2) use
-- them if it wants. is_inbox_tag flags tags that should mark docs as
-- needs-review; imported as-is.
CREATE TABLE tags (
    id                   INTEGER PRIMARY KEY,
    name                 TEXT NOT NULL UNIQUE,
    slug                 TEXT NOT NULL UNIQUE,
    color                TEXT NOT NULL DEFAULT '#a6cee3',
    matching_algorithm   INTEGER NOT NULL DEFAULT 0,
    match                TEXT NOT NULL DEFAULT '',
    is_insensitive       INTEGER NOT NULL DEFAULT 1,
    is_inbox_tag         INTEGER NOT NULL DEFAULT 0,
    created_at           INTEGER NOT NULL,
    updated_at           INTEGER NOT NULL
) STRICT;

CREATE TABLE correspondents (
    id                   INTEGER PRIMARY KEY,
    name                 TEXT NOT NULL UNIQUE,
    slug                 TEXT NOT NULL UNIQUE,
    matching_algorithm   INTEGER NOT NULL DEFAULT 0,
    match                TEXT NOT NULL DEFAULT '',
    is_insensitive       INTEGER NOT NULL DEFAULT 1,
    created_at           INTEGER NOT NULL,
    updated_at           INTEGER NOT NULL
) STRICT;

CREATE TABLE document_types (
    id                   INTEGER PRIMARY KEY,
    name                 TEXT NOT NULL UNIQUE,
    slug                 TEXT NOT NULL UNIQUE,
    matching_algorithm   INTEGER NOT NULL DEFAULT 0,
    match                TEXT NOT NULL DEFAULT '',
    is_insensitive       INTEGER NOT NULL DEFAULT 1,
    created_at           INTEGER NOT NULL,
    updated_at           INTEGER NOT NULL
) STRICT;

-- storage_paths.path is the Gonja/Jinja2 template applied at rendered-
-- view time. NOT a filesystem path.
CREATE TABLE storage_paths (
    id                   INTEGER PRIMARY KEY,
    name                 TEXT NOT NULL UNIQUE,
    slug                 TEXT NOT NULL UNIQUE,
    path                 TEXT NOT NULL,
    matching_algorithm   INTEGER NOT NULL DEFAULT 0,
    match                TEXT NOT NULL DEFAULT '',
    is_insensitive       INTEGER NOT NULL DEFAULT 1,
    created_at           INTEGER NOT NULL,
    updated_at           INTEGER NOT NULL
) STRICT;

-- Custom fields: user-defined typed attributes. data_type is the closed
-- vocabulary the design doc calls out (select, date, text, number, multi).
-- We add 'bool' / 'monetary' / 'url' / 'documentlink' so Paperless imports
-- lose zero data. The dispatcher (Phase 3) fans out by data_type.
CREATE TABLE custom_fields (
    id           INTEGER PRIMARY KEY,
    name         TEXT NOT NULL UNIQUE,
    data_type    TEXT NOT NULL CHECK (data_type IN (
        'text','number','date','bool','select','multi','url','monetary','documentlink'
    )),
    -- For 'select' and 'multi', extra_data holds the choices as JSON.
    extra_data   TEXT NOT NULL DEFAULT '{}',
    created_at   INTEGER NOT NULL,
    updated_at   INTEGER NOT NULL
) STRICT;

-- Notes: user-authored, per-document. Kept plain-text; markdown rendering
-- (if any) is a UI-side concern.
CREATE TABLE notes (
    id           INTEGER PRIMARY KEY,
    document_id  INTEGER NOT NULL REFERENCES documents(id) ON DELETE CASCADE,
    user_id      INTEGER REFERENCES users(id) ON DELETE SET NULL,
    note         TEXT NOT NULL,
    created_at   INTEGER NOT NULL
) STRICT;
CREATE INDEX notes_document ON notes(document_id);

-- ---------- document extensions ----------
--
-- SQLite ALTER TABLE ADD COLUMN is the boring way — cheap, atomic, and
-- rejects NOT NULL without DEFAULT (which is exactly what we want for
-- optional metadata: NULL means "not set", not "someone forgot to
-- backfill"). Every added column is nullable on purpose.

ALTER TABLE documents ADD COLUMN content              TEXT;                 -- OCR text; FTS5 mirrors this
ALTER TABLE documents ADD COLUMN mime_type            TEXT;                 -- server-sniffed, never trusted from upload
ALTER TABLE documents ADD COLUMN correspondent_id     INTEGER REFERENCES correspondents(id) ON DELETE SET NULL;
ALTER TABLE documents ADD COLUMN document_type_id     INTEGER REFERENCES document_types(id) ON DELETE SET NULL;
ALTER TABLE documents ADD COLUMN storage_path_id      INTEGER REFERENCES storage_paths(id) ON DELETE SET NULL;
ALTER TABLE documents ADD COLUMN added_at             INTEGER;              -- when the row was created; created_at is doc date
ALTER TABLE documents ADD COLUMN paperless_id_legacy  INTEGER;              -- one-way pointer back to a Paperless-ngx import
ALTER TABLE documents ADD COLUMN archive_serial_number INTEGER;             -- Paperless "ASN"; sparse, user-managed

-- Unique when set. Partial-index UNIQUE is how SQLite says "nullable
-- unique" without breaking on multiple NULLs.
CREATE UNIQUE INDEX documents_paperless_legacy_uniq
    ON documents(paperless_id_legacy) WHERE paperless_id_legacy IS NOT NULL;
CREATE UNIQUE INDEX documents_asn_uniq
    ON documents(archive_serial_number) WHERE archive_serial_number IS NOT NULL;

-- Lookup indexes on the new FKs. Not unique — many docs per correspondent, etc.
CREATE INDEX documents_correspondent ON documents(correspondent_id);
CREATE INDEX documents_document_type ON documents(document_type_id);
CREATE INDEX documents_storage_path  ON documents(storage_path_id);

-- ---------- junction ----------

CREATE TABLE document_tags (
    document_id  INTEGER NOT NULL REFERENCES documents(id) ON DELETE CASCADE,
    tag_id       INTEGER NOT NULL REFERENCES tags(id) ON DELETE CASCADE,
    PRIMARY KEY (document_id, tag_id)
) STRICT;
CREATE INDEX document_tags_tag ON document_tags(tag_id);

-- ---------- custom field values ----------
--
-- One row per (document, field). Type-native columns because SQLite typing
-- is dynamic anyway and this avoids the "string-encode everything" pattern
-- that then loses type information at read time.

CREATE TABLE document_custom_field_values (
    id            INTEGER PRIMARY KEY,
    document_id   INTEGER NOT NULL REFERENCES documents(id) ON DELETE CASCADE,
    field_id      INTEGER NOT NULL REFERENCES custom_fields(id) ON DELETE CASCADE,
    value_text    TEXT,
    value_number  REAL,
    value_int     INTEGER,
    value_bool    INTEGER,
    value_date    INTEGER,       -- unix epoch seconds
    UNIQUE (document_id, field_id)
) STRICT;
CREATE INDEX dcfv_field ON document_custom_field_values(field_id);
