-- suchi: rebuild-tables

-- The fifth compatibility step is the complete stable-v1 cutover. It is kept
-- as one transaction so an archive never exposes a half-migrated identity,
-- correspondent, producer, or retention contract.

-- Database-managed watched-folder ownership is an immutable user id. A blank
-- legacy value disables the source; every other legacy value must resolve
-- unambiguously or the migration aborts.
CREATE TEMP TABLE _suchi_fs_owner_guard (
    ok INTEGER NOT NULL CHECK (ok = 1)
);
INSERT INTO _suchi_fs_owner_guard(ok)
SELECT CASE
    WHEN NOT EXISTS (
        SELECT 1 FROM settings WHERE key = 'ingest.fs_watch_owner'
    ) THEN 1
    WHEN EXISTS (
        SELECT 1 FROM settings WHERE key = 'ingest.fs_watch_owner_id'
    ) THEN 0
    WHEN (
        SELECT CASE
            WHEN json_valid(value_json) = 0 THEN 0
            WHEN json_type(value_json) != 'text' THEN 0
            WHEN trim(json_extract(value_json, '$')) = '' THEN 1
            WHEN (
                SELECT count(*)
                FROM users
                WHERE lower(trim(email)) = lower(trim(json_extract(
                    (SELECT value_json FROM settings
                     WHERE key = 'ingest.fs_watch_owner'), '$'
                )))
            ) = 1 THEN 1
            ELSE 0
        END
        FROM settings
        WHERE key = 'ingest.fs_watch_owner'
    ) = 1 THEN 1
    ELSE 0
END;

INSERT INTO settings(key, value_json, updated_at)
SELECT 'ingest.fs_watch_owner_id', CAST(u.id AS TEXT), legacy.updated_at
FROM settings AS legacy
JOIN users AS u
  ON lower(trim(u.email)) = lower(trim(json_extract(legacy.value_json, '$')))
WHERE legacy.key = 'ingest.fs_watch_owner'
  AND trim(json_extract(legacy.value_json, '$')) != '';

DELETE FROM settings WHERE key = 'ingest.fs_watch_owner';
DROP TABLE _suchi_fs_owner_guard;

-- plugin_kv never became an extension contract. Refuse to discard data or
-- extension DDL that depends on it; an empty, unextended table is dead weight.
CREATE TEMP TABLE _suchi_plugin_kv_guard (
    ok INTEGER NOT NULL CHECK (ok = 1)
);
INSERT INTO _suchi_plugin_kv_guard(ok)
SELECT CASE
    WHEN EXISTS (SELECT 1 FROM plugin_kv) THEN 0
    WHEN EXISTS (
        SELECT 1
        FROM sqlite_schema
        WHERE name != 'plugin_kv'
          AND sql IS NOT NULL
          AND (
              tbl_name = 'plugin_kv'
              OR instr(lower(sql), 'plugin_kv') > 0
          )
    ) THEN 0
    ELSE 1
END;
DROP TABLE plugin_kv;
DROP TABLE _suchi_plugin_kv_guard;

DROP INDEX object_acls_lookup;

-- Add the stable OIDC binding and explicit development-account marker without
-- changing durable user IDs.
CREATE TEMP TABLE _suchi_users_identity_copy AS
SELECT id, email, display_name, role, disabled, password_hash, created_at,
       updated_at, avatar_sha, capabilities
FROM users;

DROP TABLE users;
CREATE TABLE users (
    id           INTEGER PRIMARY KEY,
    email        TEXT NOT NULL UNIQUE,
    display_name TEXT NOT NULL,
    role         TEXT NOT NULL CHECK (role IN ('admin','member')),
    disabled     INTEGER NOT NULL DEFAULT 0,
    dev_seeded   INTEGER NOT NULL DEFAULT 0 CHECK (dev_seeded IN (0,1)),
    -- Local-auth stores an argon2id hash here. OIDC-only users have this NULL.
    password_hash TEXT,
    created_at   INTEGER NOT NULL,
    updated_at   INTEGER NOT NULL,
    avatar_sha   TEXT,
    capabilities TEXT NOT NULL DEFAULT '[]',
    oidc_issuer  TEXT,
    oidc_subject TEXT,
    CHECK (
        (oidc_issuer IS NULL AND oidc_subject IS NULL)
        OR (oidc_issuer IS NOT NULL AND oidc_subject IS NOT NULL
            AND length(oidc_issuer) > 0 AND length(oidc_subject) > 0)
    )
) STRICT;

INSERT INTO users (
    id, email, display_name, role, disabled, dev_seeded, password_hash,
    created_at, updated_at, avatar_sha, capabilities, oidc_issuer, oidc_subject
)
SELECT id, email, display_name, role, disabled, 0, password_hash,
       created_at, updated_at, avatar_sha, capabilities, NULL, NULL
FROM _suchi_users_identity_copy;
DROP TABLE _suchi_users_identity_copy;

CREATE UNIQUE INDEX users_oidc_identity
    ON users(oidc_issuer, oidc_subject) WHERE oidc_issuer IS NOT NULL;

CREATE TRIGGER users_default_system_demotion AFTER UPDATE OF role ON users
WHEN OLD.role='admin' AND NEW.role='member' AND EXISTS (SELECT 1 FROM jd_systems WHERE id=1 AND code='')
BEGIN INSERT OR IGNORE INTO jd_system_members(system_id,user_id,created_at) VALUES(1,NEW.id,NEW.updated_at); END;

CREATE TRIGGER users_default_system_insert AFTER INSERT ON users
WHEN NEW.role='member' AND EXISTS (SELECT 1 FROM jd_systems WHERE id=1 AND code='')
BEGIN INSERT INTO jd_system_members(system_id,user_id,created_at) VALUES(1,NEW.id,NEW.created_at); END;

-- Retained security records share the audit table but are excluded from
-- ordinary feed pruning. Existing beta records remain ordinary rows.
CREATE TEMP TABLE _suchi_audit_retention_copy AS
SELECT id, ts, actor_kind, actor_id, action, object_kind, object_id, before_json,
       after_json, request_id, system_id
FROM audit_events;

DROP TABLE audit_events;
CREATE TABLE "audit_events" (
    id           INTEGER PRIMARY KEY,
    ts           INTEGER NOT NULL,
    actor_kind   TEXT NOT NULL,
    actor_id     INTEGER,
    action       TEXT NOT NULL,
    object_kind  TEXT NOT NULL,
    object_id    INTEGER,
    before_json  TEXT,
    after_json   TEXT,
    request_id   TEXT,
    system_id INTEGER REFERENCES jd_systems(id),
    retained     INTEGER NOT NULL DEFAULT 0 CHECK (retained IN (0,1))
) STRICT;

INSERT INTO audit_events (
    id, ts, actor_kind, actor_id, action, object_kind, object_id, before_json,
    after_json, request_id, system_id, retained
)
SELECT id, ts, actor_kind, actor_id, action, object_kind, object_id, before_json,
       after_json, request_id, system_id, 0
FROM _suchi_audit_retention_copy;
DROP TABLE _suchi_audit_retention_copy;

CREATE INDEX audit_actor ON audit_events(actor_kind, actor_id);

CREATE INDEX audit_object ON audit_events(object_kind, object_id);

CREATE INDEX audit_system ON audit_events(system_id,id);

CREATE INDEX audit_ts ON audit_events(ts);

CREATE TRIGGER audit_events_system_immutable BEFORE UPDATE OF system_id ON audit_events
WHEN NEW.system_id IS NOT OLD.system_id
BEGIN SELECT RAISE(ABORT,'system ownership is immutable'); END;

CREATE TRIGGER audit_events_system_replace BEFORE INSERT ON audit_events
WHEN EXISTS (SELECT 1 FROM audit_events WHERE id=NEW.id AND system_id IS NOT NEW.system_id)
BEGIN SELECT RAISE(ABORT,'system ownership is immutable'); END;

-- The junction table is the only correspondent source of truth. Backfill the
-- legacy primary as the first sender, deterministically resequence senders,
-- and keep every stored revision value unchanged during data movement.
DROP TRIGGER document_correspondents_revision_delete;
DROP TRIGGER document_correspondents_revision_insert;
DROP TRIGGER document_correspondents_revision_update;

INSERT OR IGNORE INTO document_correspondents(
    document_id, correspondent_id, role, position
)
SELECT id, correspondent_id, 'sender', 0
FROM documents
WHERE correspondent_id IS NOT NULL;

WITH ranked AS (
    SELECT dc.document_id, dc.correspondent_id, dc.role,
           row_number() OVER (
               PARTITION BY dc.document_id, dc.role
               ORDER BY
                   CASE WHEN dc.correspondent_id = d.correspondent_id
                        THEN 0 ELSE 1 END,
                   dc.position,
                   dc.correspondent_id
           ) - 1 AS new_position
    FROM document_correspondents AS dc
    JOIN documents AS d ON d.id = dc.document_id
    WHERE dc.role = 'sender'
)
UPDATE document_correspondents
SET position = (
    SELECT ranked.new_position
    FROM ranked
    WHERE ranked.document_id = document_correspondents.document_id
      AND ranked.correspondent_id = document_correspondents.correspondent_id
      AND ranked.role = document_correspondents.role
)
WHERE role = 'sender'
  AND position != (
      SELECT ranked.new_position
      FROM ranked
      WHERE ranked.document_id = document_correspondents.document_id
        AND ranked.correspondent_id = document_correspondents.correspondent_id
        AND ranked.role = document_correspondents.role
  );

CREATE TEMP TABLE _suchi_documents_copy AS
SELECT id, owner_id, original_blob, original_size, archive_blob, archive_size,
       title, jd_category_id, created_at, updated_at, trashed_at, content,
       mime_type, document_type_id, storage_path_id, added_at, legacy_id,
       archive_serial_number, previous_version_id, split_parent_id, split_index,
       encryption_state, decrypted_blob, decrypted_size, email_parent_id,
       email_message_id, sensitivity, thumb_sha, pipeline_version_ocr,
       pipeline_version_llm, pipeline_version_content, languages,
       languages_locked, source_mtime, content_source,
       device_content_confidence, device_ocr_language,
       device_content_received_at, split_origin_id, system_id, source_revision,
       title_revision, correspondent_revision, document_type_revision,
       category_revision, tags_revision, language_revision
FROM documents;

DROP TABLE documents;
CREATE TABLE "documents" (
    id             INTEGER PRIMARY KEY AUTOINCREMENT,
    owner_id       INTEGER NOT NULL REFERENCES users(id),
    original_blob  TEXT NOT NULL,
    original_size  INTEGER NOT NULL,
    archive_blob   TEXT,
    archive_size   INTEGER,
    title          TEXT NOT NULL DEFAULT '',
    jd_category_id INTEGER NOT NULL,
    created_at     INTEGER NOT NULL,
    updated_at     INTEGER NOT NULL,
    trashed_at     INTEGER,
    content              TEXT,
    mime_type            TEXT,
    document_type_id     INTEGER REFERENCES document_types(id) ON DELETE SET NULL,
    storage_path_id      INTEGER REFERENCES storage_paths(id) ON DELETE SET NULL,
    added_at             INTEGER,
    legacy_id            INTEGER,
    archive_serial_number INTEGER,
    previous_version_id INTEGER
    REFERENCES documents(id) ON DELETE SET NULL,
    split_parent_id INTEGER
    REFERENCES documents(id) ON DELETE SET NULL,
    split_index INTEGER,
    encryption_state TEXT
    CHECK (encryption_state IN ('encrypted', 'decrypted')),
    decrypted_blob TEXT,
    decrypted_size INTEGER,
    email_parent_id  INTEGER
    REFERENCES documents(id) ON DELETE SET NULL,
    email_message_id TEXT,
    sensitivity TEXT,
    thumb_sha TEXT,
    pipeline_version_ocr     INTEGER NOT NULL DEFAULT 0,
    pipeline_version_llm     INTEGER NOT NULL DEFAULT 0,
    pipeline_version_content INTEGER NOT NULL DEFAULT 0,
    languages        TEXT    NOT NULL DEFAULT '',
    languages_locked INTEGER NOT NULL DEFAULT 0,
    source_mtime INTEGER,
    content_source TEXT NOT NULL DEFAULT ''
    CHECK (content_source IN ('', 'device_ocr', 'server')),
    device_content_confidence REAL
    CHECK (device_content_confidence BETWEEN 0 AND 1),
    device_ocr_language TEXT NOT NULL DEFAULT '',
    device_content_received_at INTEGER,
    split_origin_id INTEGER NOT NULL DEFAULT 0
    CHECK (split_origin_id >= 0),
    system_id INTEGER NOT NULL REFERENCES jd_systems(id), source_revision INTEGER NOT NULL DEFAULT 0, title_revision INTEGER NOT NULL DEFAULT 0, correspondent_revision INTEGER NOT NULL DEFAULT 0, document_type_revision INTEGER NOT NULL DEFAULT 0, category_revision INTEGER NOT NULL DEFAULT 0, tags_revision INTEGER NOT NULL DEFAULT 0, language_revision INTEGER NOT NULL DEFAULT 0,
    FOREIGN KEY(jd_category_id,system_id) REFERENCES jd_categories(id,system_id)
) STRICT;

INSERT INTO documents(
    id, owner_id, original_blob, original_size, archive_blob, archive_size,
    title, jd_category_id, created_at, updated_at, trashed_at, content,
    mime_type, document_type_id, storage_path_id, added_at, legacy_id,
    archive_serial_number, previous_version_id, split_parent_id, split_index,
    encryption_state, decrypted_blob, decrypted_size, email_parent_id,
    email_message_id, sensitivity, thumb_sha, pipeline_version_ocr,
    pipeline_version_llm, pipeline_version_content, languages,
    languages_locked, source_mtime, content_source,
    device_content_confidence, device_ocr_language,
    device_content_received_at, split_origin_id, system_id, source_revision,
    title_revision, correspondent_revision, document_type_revision,
    category_revision, tags_revision, language_revision
)
SELECT id, owner_id, original_blob, original_size, archive_blob, archive_size,
       title, jd_category_id, created_at, updated_at, trashed_at, content,
       mime_type, document_type_id, storage_path_id, added_at, legacy_id,
       archive_serial_number, previous_version_id, split_parent_id, split_index,
       encryption_state, decrypted_blob, decrypted_size, email_parent_id,
       email_message_id, sensitivity, thumb_sha, pipeline_version_ocr,
       pipeline_version_llm, pipeline_version_content, languages,
       languages_locked, source_mtime, content_source,
       device_content_confidence, device_ocr_language,
       device_content_received_at, split_origin_id, system_id, source_revision,
       title_revision, correspondent_revision, document_type_revision,
       category_revision, tags_revision, language_revision
FROM _suchi_documents_copy;
DROP TABLE _suchi_documents_copy;

CREATE UNIQUE INDEX documents_asn_uniq
    ON documents(system_id, archive_serial_number) WHERE archive_serial_number IS NOT NULL;

CREATE INDEX documents_document_type ON documents(document_type_id);

CREATE INDEX documents_email_message_id
    ON documents(system_id, email_message_id)
    WHERE email_message_id IS NOT NULL;

CREATE INDEX documents_email_parent
    ON documents(email_parent_id)
    WHERE email_parent_id IS NOT NULL;

CREATE INDEX documents_encryption_state
    ON documents(encryption_state)
    WHERE encryption_state = 'encrypted';

CREATE INDEX documents_jd ON documents(jd_category_id);

CREATE INDEX documents_languages ON documents(languages) WHERE languages != '';

CREATE UNIQUE INDEX documents_legacy_id_uniq
    ON documents(system_id, legacy_id) WHERE legacy_id IS NOT NULL;

CREATE INDEX documents_live_created
    ON documents(created_at DESC, id DESC)
    WHERE trashed_at IS NULL;

CREATE INDEX documents_owner ON documents(owner_id);

CREATE UNIQUE INDEX documents_owner_original_blob
    ON documents(system_id, owner_id, original_blob)
    WHERE trashed_at IS NULL;

CREATE INDEX documents_previous_version
    ON documents(previous_version_id)
    WHERE previous_version_id IS NOT NULL;

CREATE UNIQUE INDEX documents_split_origin_part
    ON documents(split_origin_id, split_index)
    WHERE split_origin_id != 0;

CREATE UNIQUE INDEX documents_split_parent_part
    ON documents(split_parent_id, split_index)
    WHERE split_parent_id IS NOT NULL;

CREATE INDEX documents_storage_path  ON documents(storage_path_id);

CREATE INDEX documents_system_live ON documents(system_id,created_at DESC,id DESC) WHERE trashed_at IS NULL;

CREATE TRIGGER documents_category_revision AFTER UPDATE OF jd_category_id ON documents
BEGIN UPDATE documents SET category_revision=category_revision+1 WHERE id=NEW.id; END;

CREATE TRIGGER documents_document_type_id_insert BEFORE INSERT ON documents
WHEN NEW.document_type_id IS NOT NULL AND NEW.system_id IS NOT (SELECT system_id FROM document_types WHERE id=NEW.document_type_id)
BEGIN SELECT RAISE(ABORT,'cross-system document type id'); END;

CREATE TRIGGER documents_document_type_id_update BEFORE UPDATE OF system_id,document_type_id ON documents
WHEN NEW.document_type_id IS NOT NULL AND NEW.system_id IS NOT (SELECT system_id FROM document_types WHERE id=NEW.document_type_id)
BEGIN SELECT RAISE(ABORT,'cross-system document type id'); END;

CREATE TRIGGER documents_document_type_revision AFTER UPDATE OF document_type_id ON documents
BEGIN UPDATE documents SET document_type_revision=document_type_revision+1 WHERE id=NEW.id; END;

CREATE TRIGGER documents_email_parent_id_insert BEFORE INSERT ON documents
WHEN NEW.email_parent_id IS NOT NULL AND NEW.system_id IS NOT (SELECT system_id FROM documents WHERE id=NEW.email_parent_id)
BEGIN SELECT RAISE(ABORT,'cross-system email parent id'); END;

CREATE TRIGGER documents_email_parent_id_update BEFORE UPDATE OF system_id,email_parent_id ON documents
WHEN NEW.email_parent_id IS NOT NULL AND NEW.system_id IS NOT (SELECT system_id FROM documents WHERE id=NEW.email_parent_id)
BEGIN SELECT RAISE(ABORT,'cross-system email parent id'); END;

CREATE TRIGGER documents_fts_ad AFTER DELETE ON documents BEGIN
    INSERT INTO documents_fts(documents_fts, rowid, title, content)
    VALUES ('delete', old.id, old.title, coalesce(old.content, ''));
END;

CREATE TRIGGER documents_fts_ai AFTER INSERT ON documents BEGIN
    INSERT INTO documents_fts(rowid, title, content)
    VALUES (new.id, new.title, coalesce(new.content, ''));
END;

CREATE TRIGGER documents_fts_au AFTER UPDATE OF title, content ON documents BEGIN
    INSERT INTO documents_fts(documents_fts, rowid, title, content)
    VALUES ('delete', old.id, old.title, coalesce(old.content, ''));
    INSERT INTO documents_fts(rowid, title, content)
    VALUES (new.id, new.title, coalesce(new.content, ''));
END;

CREATE TRIGGER documents_language_revision AFTER UPDATE OF languages,languages_locked ON documents
BEGIN UPDATE documents SET language_revision=language_revision+1 WHERE id=NEW.id; END;

CREATE TRIGGER documents_previous_version_id_insert BEFORE INSERT ON documents
WHEN NEW.previous_version_id IS NOT NULL AND NEW.system_id IS NOT (SELECT system_id FROM documents WHERE id=NEW.previous_version_id)
BEGIN SELECT RAISE(ABORT,'cross-system previous version id'); END;

CREATE TRIGGER documents_previous_version_id_update BEFORE UPDATE OF system_id,previous_version_id ON documents
WHEN NEW.previous_version_id IS NOT NULL AND NEW.system_id IS NOT (SELECT system_id FROM documents WHERE id=NEW.previous_version_id)
BEGIN SELECT RAISE(ABORT,'cross-system previous version id'); END;

CREATE TRIGGER documents_source_revision AFTER UPDATE OF content,original_blob,owner_id,system_id ON documents
BEGIN UPDATE documents SET source_revision=source_revision+1 WHERE id=NEW.id; END;

CREATE TRIGGER documents_split_origin_insert BEFORE INSERT ON documents
WHEN NEW.split_origin_id<>0 AND EXISTS (SELECT 1 FROM documents WHERE id=NEW.split_origin_id AND system_id<>NEW.system_id)
BEGIN SELECT RAISE(ABORT,'cross-system split origin'); END;

CREATE TRIGGER documents_split_origin_update BEFORE UPDATE OF system_id,split_origin_id ON documents
WHEN NEW.split_origin_id<>0 AND EXISTS (SELECT 1 FROM documents WHERE id=NEW.split_origin_id AND system_id<>NEW.system_id)
BEGIN SELECT RAISE(ABORT,'cross-system split origin'); END;

CREATE TRIGGER documents_split_parent_id_insert BEFORE INSERT ON documents
WHEN NEW.split_parent_id IS NOT NULL AND NEW.system_id IS NOT (SELECT system_id FROM documents WHERE id=NEW.split_parent_id)
BEGIN SELECT RAISE(ABORT,'cross-system split parent id'); END;

CREATE TRIGGER documents_split_parent_id_update BEFORE UPDATE OF system_id,split_parent_id ON documents
WHEN NEW.split_parent_id IS NOT NULL AND NEW.system_id IS NOT (SELECT system_id FROM documents WHERE id=NEW.split_parent_id)
BEGIN SELECT RAISE(ABORT,'cross-system split parent id'); END;

CREATE TRIGGER documents_storage_path_id_insert BEFORE INSERT ON documents
WHEN NEW.storage_path_id IS NOT NULL AND NEW.system_id IS NOT (SELECT system_id FROM storage_paths WHERE id=NEW.storage_path_id)
BEGIN SELECT RAISE(ABORT,'cross-system storage path id'); END;

CREATE TRIGGER documents_storage_path_id_update BEFORE UPDATE OF system_id,storage_path_id ON documents
WHEN NEW.storage_path_id IS NOT NULL AND NEW.system_id IS NOT (SELECT system_id FROM storage_paths WHERE id=NEW.storage_path_id)
BEGIN SELECT RAISE(ABORT,'cross-system storage path id'); END;

CREATE TRIGGER documents_system_immutable BEFORE UPDATE OF system_id ON documents
WHEN NEW.system_id IS NOT OLD.system_id
BEGIN SELECT RAISE(ABORT,'system ownership is immutable'); END;

CREATE TRIGGER documents_system_replace BEFORE INSERT ON documents
WHEN EXISTS (SELECT 1 FROM documents WHERE id=NEW.id AND system_id IS NOT NEW.system_id)
BEGIN SELECT RAISE(ABORT,'system ownership is immutable'); END;

CREATE TRIGGER documents_title_revision AFTER UPDATE OF title ON documents
BEGIN UPDATE documents SET title_revision=title_revision+1 WHERE id=NEW.id; END;

CREATE TRIGGER document_correspondents_revision_delete AFTER DELETE ON document_correspondents
BEGIN UPDATE documents SET correspondent_revision=correspondent_revision+1 WHERE id=OLD.document_id; END;

CREATE TRIGGER document_correspondents_revision_insert AFTER INSERT ON document_correspondents
BEGIN UPDATE documents SET correspondent_revision=correspondent_revision+1 WHERE id=NEW.document_id; END;

CREATE TRIGGER document_correspondents_revision_update AFTER UPDATE ON document_correspondents
BEGIN
 UPDATE documents SET correspondent_revision=correspondent_revision+1 WHERE id=OLD.document_id;
 UPDATE documents SET correspondent_revision=correspondent_revision+1 WHERE id=NEW.document_id AND NEW.document_id<>OLD.document_id;
END;

-- The approval-recovery migrations have already consumed historical outbox
-- rows. Keep the exact seven-day boundary for recent operator diagnostics.
DELETE FROM jobs
WHERE state = 'done'
  AND updated_at < unixepoch() - 604800;
