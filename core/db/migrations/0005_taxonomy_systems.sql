-- suchi: rebuild-tables
-- Taxonomy upgrade from beta.2 after the mobile schema steps 0003 and 0004.
-- The migration runner disables FK enforcement on its exclusively held writer
-- before BEGIN, checks the rebuilt graph, and restores enforcement afterwards.

CREATE TABLE jd_systems (
    id INTEGER PRIMARY KEY,
    code TEXT NOT NULL UNIQUE CHECK ((id=1 AND code='') OR (length(CAST(code AS BLOB))=3 AND code GLOB '[A-Z][0-9][0-9]')),
    name TEXT NOT NULL CHECK (length(name) BETWEEN 1 AND 80 AND length(trim(name))>0),
    taxonomy TEXT NOT NULL CHECK (taxonomy IN ('jd','flat')),
    inbox_category_id INTEGER,
    preset_id TEXT,
    preset_version INTEGER,
    preset_sha256 TEXT,
    authoring_json TEXT,
    created_at INTEGER NOT NULL,
    updated_at INTEGER NOT NULL,
    FOREIGN KEY (inbox_category_id,id) REFERENCES jd_categories(id,system_id) DEFERRABLE INITIALLY DEFERRED
) STRICT;

INSERT INTO jd_systems(id,code,name,taxonomy,preset_id,preset_version,preset_sha256,authoring_json,created_at,updated_at)
SELECT 1, '',
    COALESCE((SELECT json_extract(value_json,'$.name') FROM settings
        WHERE key='taxonomy_authoring' AND json_valid(value_json)
          AND length(json_extract(value_json,'$.name')) BETWEEN 1 AND 80
          AND length(trim(json_extract(value_json,'$.name')))>0), 'Archive'),
    COALESCE((SELECT json_extract(value_json,'$') FROM settings
        WHERE key='taxonomy' AND json_valid(value_json)
          AND json_extract(value_json,'$') IN ('jd','flat')), 'jd'),
    COALESCE((SELECT json_extract(value_json,'$') FROM settings WHERE key='taxonomy_preset_id' AND json_valid(value_json)),
             (SELECT json_extract(value_json,'$') FROM settings WHERE key='preset' AND json_valid(value_json))),
    (SELECT json_extract(value_json,'$') FROM settings WHERE key='taxonomy_preset_version' AND json_valid(value_json)),
    (SELECT json_extract(value_json,'$') FROM settings WHERE key='taxonomy_preset_sha256' AND json_valid(value_json)),
    (SELECT value_json FROM settings WHERE key='taxonomy_authoring'),
    unixepoch(), unixepoch();

-- Keep FTS content and its row IDs untouched. Reinstall its triggers only after
-- the document copy, so the existing FTS index is neither erased nor duplicated.
DROP TRIGGER documents_fts_ad;
DROP TRIGGER documents_fts_ai;
DROP TRIGGER documents_fts_au;

-- Retain the high-water mark of existing AUTOINCREMENT mailboxes even if their
-- highest numbered row was previously deleted.
CREATE TEMP TABLE taxonomy_system_sequences AS
    SELECT name,seq FROM sqlite_sequence WHERE name='email_accounts';

CREATE TABLE jd_areas_new (
    code_start   INTEGER NOT NULL,
    code_end     INTEGER NOT NULL,
    name         TEXT NOT NULL,
    description  TEXT,
    position     INTEGER NOT NULL,
    system_id INTEGER NOT NULL REFERENCES jd_systems(id),
    PRIMARY KEY(system_id,code_start)
) STRICT;
INSERT INTO jd_areas_new (code_start, code_end, name, description, position, system_id)
SELECT code_start, code_end, name, description, position, 1 FROM jd_areas;
DROP TABLE jd_areas;
ALTER TABLE jd_areas_new RENAME TO jd_areas;

CREATE TABLE jd_categories_new (
    id           INTEGER PRIMARY KEY,
    area_start   INTEGER NOT NULL,
    code         INTEGER NOT NULL,
    name         TEXT NOT NULL,
    description  TEXT,
    system       INTEGER NOT NULL DEFAULT 0,
    system_id INTEGER NOT NULL REFERENCES jd_systems(id),
    CHECK (code BETWEEN area_start AND area_start + 9),
    UNIQUE(system_id,code),
    UNIQUE(id,system_id),
    FOREIGN KEY(system_id,area_start) REFERENCES jd_areas(system_id,code_start)
) STRICT;
INSERT INTO jd_categories_new (id, area_start, code, name, description, system, system_id)
SELECT id, area_start, code, name, description, system, 1 FROM jd_categories;
DROP TABLE jd_categories;
ALTER TABLE jd_categories_new RENAME TO jd_categories;

CREATE TABLE documents_new (
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
    correspondent_id     INTEGER REFERENCES correspondents(id) ON DELETE SET NULL,
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
    system_id INTEGER NOT NULL REFERENCES jd_systems(id),
    FOREIGN KEY(jd_category_id,system_id) REFERENCES jd_categories(id,system_id)
) STRICT;
INSERT INTO documents_new (id, owner_id, original_blob, original_size, archive_blob, archive_size, title, jd_category_id, created_at, updated_at, trashed_at, content, mime_type, correspondent_id, document_type_id, storage_path_id, added_at, legacy_id, archive_serial_number, previous_version_id, split_parent_id, split_index, encryption_state, decrypted_blob, decrypted_size, email_parent_id, email_message_id, sensitivity, thumb_sha, pipeline_version_ocr, pipeline_version_llm, pipeline_version_content, languages, languages_locked, source_mtime, content_source, device_content_confidence, device_ocr_language, device_content_received_at, split_origin_id, system_id)
SELECT id, owner_id, original_blob, original_size, archive_blob, archive_size, title, jd_category_id, created_at, updated_at, trashed_at, content, mime_type, correspondent_id, document_type_id, storage_path_id, added_at, legacy_id, archive_serial_number, previous_version_id, split_parent_id, split_index, encryption_state, decrypted_blob, decrypted_size, email_parent_id, email_message_id, sensitivity, thumb_sha, pipeline_version_ocr, pipeline_version_llm, pipeline_version_content, languages, languages_locked, source_mtime, content_source, device_content_confidence, device_ocr_language, device_content_received_at, split_origin_id, 1 FROM documents;
DROP TABLE documents;
ALTER TABLE documents_new RENAME TO documents;

CREATE TABLE tags_new (
    id                   INTEGER PRIMARY KEY,
    name                 TEXT NOT NULL,
    slug                 TEXT NOT NULL,
    color                TEXT NOT NULL DEFAULT '#a6cee3',
    matching_algorithm   INTEGER NOT NULL DEFAULT 0,
    match                TEXT NOT NULL DEFAULT '',
    is_insensitive       INTEGER NOT NULL DEFAULT 1,
    is_inbox_tag         INTEGER NOT NULL DEFAULT 0,
    created_at           INTEGER NOT NULL,
    updated_at           INTEGER NOT NULL,
    parent_id INTEGER REFERENCES tags(id) ON DELETE CASCADE,
    system_id INTEGER NOT NULL REFERENCES jd_systems(id),
    UNIQUE(system_id,name),
    UNIQUE(system_id,slug)
) STRICT;
INSERT INTO tags_new (id, name, slug, color, matching_algorithm, match, is_insensitive, is_inbox_tag, created_at, updated_at, parent_id, system_id)
SELECT id, name, slug, color, matching_algorithm, match, is_insensitive, is_inbox_tag, created_at, updated_at, parent_id, 1 FROM tags;
DROP TABLE tags;
ALTER TABLE tags_new RENAME TO tags;

CREATE TABLE correspondents_new (
    id                   INTEGER PRIMARY KEY,
    name                 TEXT NOT NULL,
    slug                 TEXT NOT NULL,
    matching_algorithm   INTEGER NOT NULL DEFAULT 0,
    match                TEXT NOT NULL DEFAULT '',
    is_insensitive       INTEGER NOT NULL DEFAULT 1,
    created_at           INTEGER NOT NULL,
    updated_at           INTEGER NOT NULL,
    system_id INTEGER NOT NULL REFERENCES jd_systems(id),
    UNIQUE(system_id,name),
    UNIQUE(system_id,slug)
) STRICT;
INSERT INTO correspondents_new (id, name, slug, matching_algorithm, match, is_insensitive, created_at, updated_at, system_id)
SELECT id, name, slug, matching_algorithm, match, is_insensitive, created_at, updated_at, 1 FROM correspondents;
DROP TABLE correspondents;
ALTER TABLE correspondents_new RENAME TO correspondents;

CREATE TABLE document_types_new (
    id                   INTEGER PRIMARY KEY,
    name                 TEXT NOT NULL,
    slug                 TEXT NOT NULL,
    matching_algorithm   INTEGER NOT NULL DEFAULT 0,
    match                TEXT NOT NULL DEFAULT '',
    is_insensitive       INTEGER NOT NULL DEFAULT 1,
    created_at           INTEGER NOT NULL,
    updated_at           INTEGER NOT NULL,
    system_id INTEGER NOT NULL REFERENCES jd_systems(id),
    UNIQUE(system_id,name),
    UNIQUE(system_id,slug)
) STRICT;
INSERT INTO document_types_new (id, name, slug, matching_algorithm, match, is_insensitive, created_at, updated_at, system_id)
SELECT id, name, slug, matching_algorithm, match, is_insensitive, created_at, updated_at, 1 FROM document_types;
DROP TABLE document_types;
ALTER TABLE document_types_new RENAME TO document_types;

CREATE TABLE storage_paths_new (
    id                   INTEGER PRIMARY KEY,
    name                 TEXT NOT NULL,
    slug                 TEXT NOT NULL,
    path                 TEXT NOT NULL,
    matching_algorithm   INTEGER NOT NULL DEFAULT 0,
    match                TEXT NOT NULL DEFAULT '',
    is_insensitive       INTEGER NOT NULL DEFAULT 1,
    created_at           INTEGER NOT NULL,
    updated_at           INTEGER NOT NULL,
    system_id INTEGER NOT NULL REFERENCES jd_systems(id),
    UNIQUE(system_id,name),
    UNIQUE(system_id,slug)
) STRICT;
INSERT INTO storage_paths_new (id, name, slug, path, matching_algorithm, match, is_insensitive, created_at, updated_at, system_id)
SELECT id, name, slug, path, matching_algorithm, match, is_insensitive, created_at, updated_at, 1 FROM storage_paths;
DROP TABLE storage_paths;
ALTER TABLE storage_paths_new RENAME TO storage_paths;

CREATE TABLE custom_fields_new (
    id           INTEGER PRIMARY KEY,
    name         TEXT NOT NULL,
    data_type    TEXT NOT NULL CHECK (data_type IN (
        'text','number','date','bool','select','multi','url','monetary','documentlink'
    )),
    extra_data   TEXT NOT NULL DEFAULT '{}',
    created_at   INTEGER NOT NULL,
    updated_at   INTEGER NOT NULL,
    system_id INTEGER NOT NULL REFERENCES jd_systems(id),
    UNIQUE(system_id,name)
) STRICT;
INSERT INTO custom_fields_new (id, name, data_type, extra_data, created_at, updated_at, system_id)
SELECT id, name, data_type, extra_data, created_at, updated_at, 1 FROM custom_fields;
DROP TABLE custom_fields;
ALTER TABLE custom_fields_new RENAME TO custom_fields;

CREATE TABLE automations_new (
    id          INTEGER PRIMARY KEY,
    name        TEXT NOT NULL,
    order_index INTEGER NOT NULL DEFAULT 0,
    enabled     INTEGER NOT NULL DEFAULT 1,
    created_at  INTEGER NOT NULL,
    updated_at  INTEGER NOT NULL,
    preset_slug TEXT,
    system_id INTEGER NOT NULL REFERENCES jd_systems(id),
    UNIQUE(system_id,name)
) STRICT;
INSERT INTO automations_new (id, name, order_index, enabled, created_at, updated_at, preset_slug, system_id)
SELECT id, name, order_index, enabled, created_at, updated_at, preset_slug, 1 FROM automations;
DROP TABLE automations;
ALTER TABLE automations_new RENAME TO automations;

CREATE TABLE saved_views_new (
    id           INTEGER PRIMARY KEY,
    owner_id     INTEGER NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    name         TEXT NOT NULL,
    filter_json  TEXT NOT NULL DEFAULT '{}',
    display      TEXT NOT NULL DEFAULT 'table',
    position     INTEGER NOT NULL DEFAULT 0,
    created_at   INTEGER NOT NULL,
    updated_at   INTEGER NOT NULL,
    shared INTEGER NOT NULL DEFAULT 0,
    system_id INTEGER NOT NULL REFERENCES jd_systems(id),
    UNIQUE(system_id, owner_id, name)
) STRICT;
INSERT INTO saved_views_new (id, owner_id, name, filter_json, display, position, created_at, updated_at, shared, system_id)
SELECT id, owner_id, name, filter_json, display, position, created_at, updated_at, shared, 1 FROM saved_views;
DROP TABLE saved_views;
ALTER TABLE saved_views_new RENAME TO saved_views;

CREATE TABLE email_accounts_new (
    id                INTEGER PRIMARY KEY AUTOINCREMENT,
    name              TEXT NOT NULL,
    owner_id          INTEGER NOT NULL REFERENCES users(id),
    provider          TEXT NOT NULL,
    host              TEXT NOT NULL,
    port              INTEGER NOT NULL,
    use_tls           INTEGER NOT NULL DEFAULT 1,
    tls_ca_file       TEXT,
    folder            TEXT NOT NULL DEFAULT 'INBOX',
    processed_folder  TEXT,
    poll_interval_min INTEGER NOT NULL DEFAULT 10,
    auth_method       TEXT NOT NULL,
    username          TEXT NOT NULL,
    sealed_secret     BLOB NOT NULL,
    oauth_account_id  TEXT,
    intake_policy     TEXT NOT NULL DEFAULT '{"rules":[{"selection":"all","content":"email_and_files"}]}',
    enabled           INTEGER NOT NULL DEFAULT 1,
    last_sync_at      INTEGER,
    last_error        TEXT,
    created_at        INTEGER NOT NULL,
    updated_at        INTEGER NOT NULL,
    sync_since        INTEGER,
    last_uid_seen     INTEGER NOT NULL DEFAULT 0,
    uidvalidity_seen  INTEGER NOT NULL DEFAULT 0,
    mark_seen         INTEGER NOT NULL DEFAULT 0,
    system_id INTEGER NOT NULL REFERENCES jd_systems(id)
);
INSERT INTO email_accounts_new (id, name, owner_id, provider, host, port, use_tls, tls_ca_file, folder, processed_folder, poll_interval_min, auth_method, username, sealed_secret, oauth_account_id, intake_policy, enabled, last_sync_at, last_error, created_at, updated_at, sync_since, last_uid_seen, uidvalidity_seen, mark_seen, system_id)
SELECT id, name, owner_id, provider, host, port, use_tls, tls_ca_file, folder, processed_folder, poll_interval_min, auth_method, username, sealed_secret, oauth_account_id, intake_policy, enabled, last_sync_at, last_error, created_at, updated_at, sync_since, last_uid_seen, uidvalidity_seen, mark_seen, 1 FROM email_accounts;
DROP TABLE email_accounts;
ALTER TABLE email_accounts_new RENAME TO email_accounts;

CREATE TABLE decryption_passwords_new (
    id            INTEGER PRIMARY KEY,
    owner_id      INTEGER NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    ciphertext    BLOB NOT NULL,
    label         TEXT,
    created_at    INTEGER NOT NULL,
    last_used_at  INTEGER,
    last_used_doc_id INTEGER,
    system_id INTEGER NOT NULL REFERENCES jd_systems(id)
) STRICT;
INSERT INTO decryption_passwords_new (id, owner_id, ciphertext, label, created_at, last_used_at, last_used_doc_id, system_id)
SELECT id, owner_id, ciphertext, label, created_at, last_used_at, last_used_doc_id, 1 FROM decryption_passwords;
DROP TABLE decryption_passwords;
ALTER TABLE decryption_passwords_new RENAME TO decryption_passwords;

CREATE TABLE share_links_new (
    id            INTEGER PRIMARY KEY,
    token         TEXT NOT NULL UNIQUE,
    doc_ids_json  TEXT NOT NULL,
    created_by    INTEGER NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    expires_at    INTEGER,
    password_hash TEXT,
    label         TEXT NOT NULL DEFAULT '',
    view_count    INTEGER NOT NULL DEFAULT 0,
    created_at    INTEGER NOT NULL,
    revoked_at    INTEGER,
    system_id INTEGER NOT NULL REFERENCES jd_systems(id)
) STRICT;
INSERT INTO share_links_new (id, token, doc_ids_json, created_by, expires_at, password_hash, label, view_count, created_at, revoked_at, system_id)
SELECT id, token, doc_ids_json, created_by, expires_at, password_hash, label, view_count, created_at, revoked_at, 1 FROM share_links;
DROP TABLE share_links;
ALTER TABLE share_links_new RENAME TO share_links;

CREATE TABLE approval_defs_new (
    id             INTEGER PRIMARY KEY,
    slug           TEXT NOT NULL,
    version        INTEGER NOT NULL,
    spec_json      TEXT NOT NULL,
    active         INTEGER NOT NULL DEFAULT 1,
    created_at     INTEGER NOT NULL,
    created_by     INTEGER REFERENCES users(id),
    system_id INTEGER NOT NULL REFERENCES jd_systems(id),
    UNIQUE(system_id, slug, version)
) STRICT;
INSERT INTO approval_defs_new (id, slug, version, spec_json, active, created_at, created_by, system_id)
SELECT id, slug, version, spec_json, active, created_at, created_by, 1 FROM approval_defs;
DROP TABLE approval_defs;
ALTER TABLE approval_defs_new RENAME TO approval_defs;

CREATE TABLE approval_runs_new (
    id               INTEGER PRIMARY KEY,
    def_id           INTEGER NOT NULL REFERENCES "approval_defs"(id),
    doc_id           INTEGER REFERENCES documents(id),
    state            TEXT NOT NULL,
    current_state    TEXT NOT NULL,
    vars_json        TEXT NOT NULL DEFAULT '{}',
    state_entered_at INTEGER NOT NULL,
    deadline_at      INTEGER,
    started_by       INTEGER REFERENCES users(id),
    started_at       INTEGER NOT NULL,
    ended_at         INTEGER,
    system_id INTEGER NOT NULL REFERENCES jd_systems(id)
) STRICT;
INSERT INTO approval_runs_new (id, def_id, doc_id, state, current_state, vars_json, state_entered_at, deadline_at, started_by, started_at, ended_at, system_id)
SELECT id, def_id, doc_id, state, current_state, vars_json, state_entered_at, deadline_at, started_by, started_at, ended_at, 1 FROM approval_runs;
DROP TABLE approval_runs;
ALTER TABLE approval_runs_new RENAME TO approval_runs;

-- Token IDs are retained by in-flight principals. Never reuse a revoked ID;
-- copying explicit IDs seeds AUTOINCREMENT above all pre-migration live tokens.
CREATE TABLE api_tokens_new (
    id           INTEGER PRIMARY KEY AUTOINCREMENT,
    user_id      INTEGER NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    name         TEXT NOT NULL,
    token_hash   TEXT NOT NULL UNIQUE,
    scopes       TEXT NOT NULL,
    created_at   INTEGER NOT NULL,
    last_used_at INTEGER,
    revoked_at   INTEGER,
    source TEXT NOT NULL DEFAULT ''
    CHECK (source IN ('', 'mobile_pairing')),
    system_id INTEGER NOT NULL REFERENCES jd_systems(id)
) STRICT;
INSERT INTO api_tokens_new (id, user_id, name, token_hash, scopes, created_at, last_used_at, revoked_at, source, system_id)
SELECT id, user_id, name, token_hash, scopes, created_at, last_used_at, revoked_at, source, 1 FROM api_tokens;
DROP TABLE api_tokens;
ALTER TABLE api_tokens_new RENAME TO api_tokens;

CREATE TABLE mobile_pairings_new (
    user_id    INTEGER PRIMARY KEY REFERENCES users(id) ON DELETE CASCADE,
    code_hash  TEXT NOT NULL UNIQUE CHECK (length(code_hash) = 64),
    name       TEXT NOT NULL CHECK (length(name) BETWEEN 1 AND 64),
    expires_at INTEGER NOT NULL,
    system_id INTEGER NOT NULL REFERENCES jd_systems(id)
) STRICT;
INSERT INTO mobile_pairings_new (user_id, code_hash, name, expires_at, system_id)
SELECT user_id, code_hash, name, expires_at, 1 FROM mobile_pairings;
DROP TABLE mobile_pairings;
ALTER TABLE mobile_pairings_new RENAME TO mobile_pairings;

CREATE TABLE jobs_new (
    id           INTEGER PRIMARY KEY,
    kind         TEXT NOT NULL,
    doc_id       INTEGER,
    payload      TEXT NOT NULL DEFAULT '{}',
    state        TEXT NOT NULL DEFAULT 'pending' CHECK (state IN ('pending','running','done','dead')),
    attempts     INTEGER NOT NULL DEFAULT 0,
    next_run_at  INTEGER NOT NULL,
    last_error   TEXT,
    created_at   INTEGER NOT NULL,
    updated_at   INTEGER NOT NULL,
    system_id INTEGER REFERENCES jd_systems(id)
) STRICT;
INSERT INTO jobs_new (id, kind, doc_id, payload, state, attempts, next_run_at, last_error, created_at, updated_at, system_id)
SELECT id, kind, doc_id, payload, state, attempts, next_run_at, last_error, created_at, updated_at, 1 FROM jobs;
DROP TABLE jobs;
ALTER TABLE jobs_new RENAME TO jobs;

CREATE TABLE audit_events_new (
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
    system_id INTEGER REFERENCES jd_systems(id)
) STRICT;
INSERT INTO audit_events_new (id, ts, actor_kind, actor_id, action, object_kind, object_id, before_json, after_json, request_id, system_id)
SELECT id, ts, actor_kind, actor_id, action, object_kind, object_id, before_json, after_json, request_id, CASE WHEN object_kind IN ('document','documents','document_intelligence','tag','correspondent','document_type','storage_path','custom_field','automation','saved_view','email_account','decryption_password','share_link','approval_def','approval_run','approval_task','approval_transition','api_token','mobile_pairing','jd_category','jd_area','taxonomy','job') OR action LIKE 'taxonomy.%' OR action LIKE 'jd.%' OR action LIKE 'approval.%' THEN 1 ELSE NULL END FROM audit_events;
DROP TABLE audit_events;
ALTER TABLE audit_events_new RENAME TO audit_events;

CREATE INDEX api_tokens_user ON api_tokens(system_id, user_id);

CREATE INDEX audit_actor ON audit_events(actor_kind, actor_id);

CREATE INDEX audit_object ON audit_events(object_kind, object_id);

CREATE INDEX audit_ts ON audit_events(ts);

CREATE INDEX decryption_passwords_owner
    ON decryption_passwords(system_id, owner_id, last_used_at DESC);

CREATE INDEX idx_email_accounts_enabled ON email_accounts(system_id, enabled);

CREATE UNIQUE INDEX documents_asn_uniq
    ON documents(system_id, archive_serial_number) WHERE archive_serial_number IS NOT NULL;

CREATE INDEX documents_correspondent ON documents(correspondent_id);

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

CREATE INDEX documents_owner ON documents(owner_id);

CREATE UNIQUE INDEX documents_owner_original_blob
    ON documents(system_id, owner_id, original_blob)
    WHERE trashed_at IS NULL;

CREATE INDEX documents_previous_version
    ON documents(previous_version_id)
    WHERE previous_version_id IS NOT NULL;

CREATE UNIQUE INDEX documents_split_parent_part
    ON documents(split_parent_id, split_index)
    WHERE split_parent_id IS NOT NULL;

CREATE INDEX documents_storage_path  ON documents(storage_path_id);

CREATE INDEX idx_approval_defs_active        ON approval_defs(system_id, active, slug);

CREATE INDEX idx_approval_runs_active        ON approval_runs(system_id, state, deadline_at);

CREATE INDEX idx_approval_runs_doc           ON approval_runs(doc_id);

CREATE INDEX idx_saved_views_owner ON saved_views(system_id, owner_id, position);

CREATE INDEX idx_share_links_creator ON share_links(system_id, created_by, created_at DESC);

CREATE INDEX idx_share_links_expires ON share_links(expires_at) WHERE expires_at IS NOT NULL;

CREATE INDEX idx_automations_enabled ON automations(system_id, enabled, order_index)
  WHERE enabled = 1;

CREATE INDEX idx_automations_preset_slug
  ON automations(system_id, preset_slug) WHERE preset_slug IS NOT NULL;

CREATE INDEX jobs_doc ON jobs(doc_id);

CREATE INDEX jobs_ready ON jobs(next_run_at) WHERE state = 'pending';

CREATE INDEX jobs_state ON jobs(state);

CREATE INDEX tags_parent ON tags(parent_id) WHERE parent_id IS NOT NULL;

CREATE INDEX documents_live_created
    ON documents(created_at DESC, id DESC)
    WHERE trashed_at IS NULL;

CREATE UNIQUE INDEX documents_split_origin_part
    ON documents(split_origin_id, split_index)
    WHERE split_origin_id != 0;

UPDATE sqlite_sequence SET seq=MAX(seq,COALESCE((SELECT seq FROM taxonomy_system_sequences WHERE name='email_accounts'),0)) WHERE name='email_accounts';
INSERT INTO sqlite_sequence(name,seq)
SELECT name,seq FROM taxonomy_system_sequences WHERE NOT EXISTS (
    SELECT 1 FROM sqlite_sequence WHERE name='email_accounts'
);
DROP TABLE taxonomy_system_sequences;

UPDATE jd_systems SET inbox_category_id=(
    SELECT c.id FROM jd_categories c JOIN settings s ON s.key='jd_inbox_category_id'
    WHERE c.system_id=1 AND c.system=1 AND json_valid(s.value_json)
      AND c.id=json_extract(s.value_json,'$')
) WHERE id=1;
DELETE FROM settings WHERE key IN ('taxonomy','jd_inbox_category_id','preset','taxonomy_preset_id','taxonomy_preset_version','taxonomy_preset_sha256','taxonomy_authoring');

UPDATE upload_idempotency SET request_fingerprint='1:' || request_fingerprint;
-- Pending, running (reclaimed at boot), and dead (manually retryable) all retain
-- IDs, attempts and scheduling history while acquiring a recoverable target.
UPDATE jobs SET system_id=1,payload='{"system_id":1}'
WHERE kind='taxonomy_index' AND state IN ('pending','running','dead');

CREATE INDEX documents_system_live ON documents(system_id,created_at DESC,id DESC) WHERE trashed_at IS NULL;
CREATE INDEX jobs_system ON jobs(system_id,id);
CREATE INDEX audit_system ON audit_events(system_id,id);

CREATE TABLE jd_system_members (
    system_id INTEGER NOT NULL REFERENCES jd_systems(id),
    user_id INTEGER NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    created_at INTEGER NOT NULL,
    PRIMARY KEY(system_id,user_id)
) STRICT;
CREATE INDEX jd_system_members_user ON jd_system_members(user_id,system_id);
INSERT INTO jd_system_members(system_id,user_id,created_at)
    SELECT 1,id,unixepoch() FROM users WHERE role<>'admin';

CREATE TRIGGER jd_systems_identity_update BEFORE UPDATE OF id,code ON jd_systems
WHEN NEW.id<>OLD.id OR (OLD.code<>'' AND NEW.code<>OLD.code)
BEGIN SELECT RAISE(ABORT,'system identity is immutable'); END;
CREATE TRIGGER jd_systems_identity_delete BEFORE DELETE ON jd_systems
BEGIN SELECT RAISE(ABORT,'system identity is permanent'); END;
CREATE TRIGGER jd_systems_inbox_insert BEFORE INSERT ON jd_systems
WHEN NEW.inbox_category_id IS NOT NULL AND NOT EXISTS (
    SELECT 1 FROM jd_categories WHERE id=NEW.inbox_category_id AND system_id=NEW.id AND system=1)
BEGIN SELECT RAISE(ABORT,'Inbox must be a protected category in its system'); END;
CREATE TRIGGER jd_systems_inbox_update BEFORE UPDATE OF inbox_category_id ON jd_systems
WHEN NEW.inbox_category_id IS NOT NULL AND NOT EXISTS (
    SELECT 1 FROM jd_categories WHERE id=NEW.inbox_category_id AND system_id=NEW.id AND system=1)
BEGIN SELECT RAISE(ABORT,'Inbox must be a protected category in its system'); END;
CREATE TRIGGER jd_systems_identity_insert BEFORE INSERT ON jd_systems
WHEN EXISTS (SELECT 1 FROM jd_systems WHERE id=NEW.id AND code<>'' AND code<>NEW.code)
    OR EXISTS (SELECT 1 FROM jd_systems WHERE code=NEW.code AND id<>NEW.id)
BEGIN SELECT RAISE(ABORT,'system identity is permanent'); END;
CREATE TRIGGER jd_categories_inbox_protected BEFORE UPDATE OF system ON jd_categories
WHEN NEW.system<>1 AND EXISTS (SELECT 1 FROM jd_systems WHERE inbox_category_id=OLD.id)
BEGIN SELECT RAISE(ABORT,'Inbox must remain protected'); END;

CREATE TRIGGER users_default_system_insert AFTER INSERT ON users
WHEN NEW.role='member' AND EXISTS (SELECT 1 FROM jd_systems WHERE id=1 AND code='')
BEGIN INSERT INTO jd_system_members(system_id,user_id,created_at) VALUES(1,NEW.id,NEW.created_at); END;
CREATE TRIGGER users_default_system_demotion AFTER UPDATE OF role ON users
WHEN OLD.role='admin' AND NEW.role='member' AND EXISTS (SELECT 1 FROM jd_systems WHERE id=1 AND code='')
BEGIN INSERT OR IGNORE INTO jd_system_members(system_id,user_id,created_at) VALUES(1,NEW.id,NEW.updated_at); END;
CREATE TRIGGER jd_system_members_identity BEFORE UPDATE OF system_id,user_id ON jd_system_members
WHEN NEW.system_id<>OLD.system_id OR NEW.user_id<>OLD.user_id
BEGIN SELECT RAISE(ABORT,'replace membership with explicit removal and grant'); END;
CREATE TRIGGER jd_system_members_revoke AFTER DELETE ON jd_system_members
BEGIN
    DELETE FROM api_tokens WHERE user_id=OLD.user_id AND system_id=OLD.system_id;
    DELETE FROM mobile_pairings WHERE user_id=OLD.user_id AND system_id=OLD.system_id;
    UPDATE share_links SET revoked_at=COALESCE(revoked_at,unixepoch()) WHERE created_by=OLD.user_id AND system_id=OLD.system_id;
END;

-- REPLACE is an INSERT followed by an implicit delete. Guard retained global
-- identities here as well as ordinary UPDATEs, regardless of recursive_triggers.
CREATE TRIGGER jd_categories_system_replace BEFORE INSERT ON jd_categories
WHEN EXISTS (SELECT 1 FROM jd_categories WHERE id=NEW.id AND system_id IS NOT NEW.system_id)
BEGIN SELECT RAISE(ABORT,'system ownership is immutable'); END;
CREATE TRIGGER documents_system_replace BEFORE INSERT ON documents
WHEN EXISTS (SELECT 1 FROM documents WHERE id=NEW.id AND system_id IS NOT NEW.system_id)
BEGIN SELECT RAISE(ABORT,'system ownership is immutable'); END;
CREATE TRIGGER tags_system_replace BEFORE INSERT ON tags
WHEN EXISTS (SELECT 1 FROM tags WHERE id=NEW.id AND system_id IS NOT NEW.system_id)
BEGIN SELECT RAISE(ABORT,'system ownership is immutable'); END;
CREATE TRIGGER correspondents_system_replace BEFORE INSERT ON correspondents
WHEN EXISTS (SELECT 1 FROM correspondents WHERE id=NEW.id AND system_id IS NOT NEW.system_id)
BEGIN SELECT RAISE(ABORT,'system ownership is immutable'); END;
CREATE TRIGGER document_types_system_replace BEFORE INSERT ON document_types
WHEN EXISTS (SELECT 1 FROM document_types WHERE id=NEW.id AND system_id IS NOT NEW.system_id)
BEGIN SELECT RAISE(ABORT,'system ownership is immutable'); END;
CREATE TRIGGER storage_paths_system_replace BEFORE INSERT ON storage_paths
WHEN EXISTS (SELECT 1 FROM storage_paths WHERE id=NEW.id AND system_id IS NOT NEW.system_id)
BEGIN SELECT RAISE(ABORT,'system ownership is immutable'); END;
CREATE TRIGGER custom_fields_system_replace BEFORE INSERT ON custom_fields
WHEN EXISTS (SELECT 1 FROM custom_fields WHERE id=NEW.id AND system_id IS NOT NEW.system_id)
BEGIN SELECT RAISE(ABORT,'system ownership is immutable'); END;
CREATE TRIGGER automations_system_replace BEFORE INSERT ON automations
WHEN EXISTS (SELECT 1 FROM automations WHERE id=NEW.id AND system_id IS NOT NEW.system_id)
BEGIN SELECT RAISE(ABORT,'system ownership is immutable'); END;
CREATE TRIGGER saved_views_system_replace BEFORE INSERT ON saved_views
WHEN EXISTS (SELECT 1 FROM saved_views WHERE id=NEW.id AND system_id IS NOT NEW.system_id)
BEGIN SELECT RAISE(ABORT,'system ownership is immutable'); END;
CREATE TRIGGER email_accounts_system_replace BEFORE INSERT ON email_accounts
WHEN EXISTS (SELECT 1 FROM email_accounts WHERE id=NEW.id AND system_id IS NOT NEW.system_id)
BEGIN SELECT RAISE(ABORT,'system ownership is immutable'); END;
CREATE TRIGGER decryption_passwords_system_replace BEFORE INSERT ON decryption_passwords
WHEN EXISTS (SELECT 1 FROM decryption_passwords WHERE id=NEW.id AND system_id IS NOT NEW.system_id)
BEGIN SELECT RAISE(ABORT,'system ownership is immutable'); END;
CREATE TRIGGER share_links_system_replace BEFORE INSERT ON share_links
WHEN EXISTS (SELECT 1 FROM share_links WHERE id=NEW.id AND system_id IS NOT NEW.system_id)
BEGIN SELECT RAISE(ABORT,'system ownership is immutable'); END;
CREATE TRIGGER approval_defs_system_replace BEFORE INSERT ON approval_defs
WHEN EXISTS (SELECT 1 FROM approval_defs WHERE id=NEW.id AND system_id IS NOT NEW.system_id)
BEGIN SELECT RAISE(ABORT,'system ownership is immutable'); END;
CREATE TRIGGER approval_runs_system_replace BEFORE INSERT ON approval_runs
WHEN EXISTS (SELECT 1 FROM approval_runs WHERE id=NEW.id AND system_id IS NOT NEW.system_id)
BEGIN SELECT RAISE(ABORT,'system ownership is immutable'); END;
CREATE TRIGGER api_tokens_system_replace BEFORE INSERT ON api_tokens
WHEN EXISTS (SELECT 1 FROM api_tokens WHERE id=NEW.id AND system_id IS NOT NEW.system_id)
BEGIN SELECT RAISE(ABORT,'system ownership is immutable'); END;
CREATE TRIGGER mobile_pairings_system_replace BEFORE INSERT ON mobile_pairings
WHEN EXISTS (SELECT 1 FROM mobile_pairings WHERE user_id=NEW.user_id AND system_id IS NOT NEW.system_id)
BEGIN SELECT RAISE(ABORT,'system ownership is immutable'); END;
CREATE TRIGGER jobs_system_replace BEFORE INSERT ON jobs
WHEN EXISTS (SELECT 1 FROM jobs WHERE id=NEW.id AND system_id IS NOT NEW.system_id)
BEGIN SELECT RAISE(ABORT,'system ownership is immutable'); END;
CREATE TRIGGER audit_events_system_replace BEFORE INSERT ON audit_events
WHEN EXISTS (SELECT 1 FROM audit_events WHERE id=NEW.id AND system_id IS NOT NEW.system_id)
BEGIN SELECT RAISE(ABORT,'system ownership is immutable'); END;

CREATE TRIGGER jd_areas_system_immutable BEFORE UPDATE OF system_id ON jd_areas
WHEN NEW.system_id IS NOT OLD.system_id
BEGIN SELECT RAISE(ABORT,'system ownership is immutable'); END;

CREATE TRIGGER jd_categories_system_immutable BEFORE UPDATE OF system_id ON jd_categories
WHEN NEW.system_id IS NOT OLD.system_id
BEGIN SELECT RAISE(ABORT,'system ownership is immutable'); END;

CREATE TRIGGER documents_system_immutable BEFORE UPDATE OF system_id ON documents
WHEN NEW.system_id IS NOT OLD.system_id
BEGIN SELECT RAISE(ABORT,'system ownership is immutable'); END;

CREATE TRIGGER tags_system_immutable BEFORE UPDATE OF system_id ON tags
WHEN NEW.system_id IS NOT OLD.system_id
BEGIN SELECT RAISE(ABORT,'system ownership is immutable'); END;

CREATE TRIGGER correspondents_system_immutable BEFORE UPDATE OF system_id ON correspondents
WHEN NEW.system_id IS NOT OLD.system_id
BEGIN SELECT RAISE(ABORT,'system ownership is immutable'); END;

CREATE TRIGGER document_types_system_immutable BEFORE UPDATE OF system_id ON document_types
WHEN NEW.system_id IS NOT OLD.system_id
BEGIN SELECT RAISE(ABORT,'system ownership is immutable'); END;

CREATE TRIGGER storage_paths_system_immutable BEFORE UPDATE OF system_id ON storage_paths
WHEN NEW.system_id IS NOT OLD.system_id
BEGIN SELECT RAISE(ABORT,'system ownership is immutable'); END;

CREATE TRIGGER custom_fields_system_immutable BEFORE UPDATE OF system_id ON custom_fields
WHEN NEW.system_id IS NOT OLD.system_id
BEGIN SELECT RAISE(ABORT,'system ownership is immutable'); END;

CREATE TRIGGER automations_system_immutable BEFORE UPDATE OF system_id ON automations
WHEN NEW.system_id IS NOT OLD.system_id
BEGIN SELECT RAISE(ABORT,'system ownership is immutable'); END;

CREATE TRIGGER saved_views_system_immutable BEFORE UPDATE OF system_id ON saved_views
WHEN NEW.system_id IS NOT OLD.system_id
BEGIN SELECT RAISE(ABORT,'system ownership is immutable'); END;

CREATE TRIGGER email_accounts_system_immutable BEFORE UPDATE OF system_id ON email_accounts
WHEN NEW.system_id IS NOT OLD.system_id
BEGIN SELECT RAISE(ABORT,'system ownership is immutable'); END;

CREATE TRIGGER decryption_passwords_system_immutable BEFORE UPDATE OF system_id ON decryption_passwords
WHEN NEW.system_id IS NOT OLD.system_id
BEGIN SELECT RAISE(ABORT,'system ownership is immutable'); END;

CREATE TRIGGER share_links_system_immutable BEFORE UPDATE OF system_id ON share_links
WHEN NEW.system_id IS NOT OLD.system_id
BEGIN SELECT RAISE(ABORT,'system ownership is immutable'); END;

CREATE TRIGGER approval_defs_system_immutable BEFORE UPDATE OF system_id ON approval_defs
WHEN NEW.system_id IS NOT OLD.system_id
BEGIN SELECT RAISE(ABORT,'system ownership is immutable'); END;

CREATE TRIGGER approval_runs_system_immutable BEFORE UPDATE OF system_id ON approval_runs
WHEN NEW.system_id IS NOT OLD.system_id
BEGIN SELECT RAISE(ABORT,'system ownership is immutable'); END;

CREATE TRIGGER api_tokens_system_immutable BEFORE UPDATE OF system_id ON api_tokens
WHEN NEW.system_id IS NOT OLD.system_id
BEGIN SELECT RAISE(ABORT,'system ownership is immutable'); END;

CREATE TRIGGER mobile_pairings_system_immutable BEFORE UPDATE OF system_id ON mobile_pairings
WHEN NEW.system_id IS NOT OLD.system_id
BEGIN SELECT RAISE(ABORT,'system ownership is immutable'); END;

CREATE TRIGGER jobs_system_immutable BEFORE UPDATE OF system_id ON jobs
WHEN NEW.system_id IS NOT OLD.system_id
BEGIN SELECT RAISE(ABORT,'system ownership is immutable'); END;

CREATE TRIGGER audit_events_system_immutable BEFORE UPDATE OF system_id ON audit_events
WHEN NEW.system_id IS NOT OLD.system_id
BEGIN SELECT RAISE(ABORT,'system ownership is immutable'); END;

CREATE TRIGGER documents_correspondent_id_insert BEFORE INSERT ON documents
WHEN NEW.correspondent_id IS NOT NULL AND NEW.system_id IS NOT (SELECT system_id FROM correspondents WHERE id=NEW.correspondent_id)
BEGIN SELECT RAISE(ABORT,'cross-system correspondent id'); END;

CREATE TRIGGER documents_correspondent_id_update BEFORE UPDATE OF system_id,correspondent_id ON documents
WHEN NEW.correspondent_id IS NOT NULL AND NEW.system_id IS NOT (SELECT system_id FROM correspondents WHERE id=NEW.correspondent_id)
BEGIN SELECT RAISE(ABORT,'cross-system correspondent id'); END;

CREATE TRIGGER documents_document_type_id_insert BEFORE INSERT ON documents
WHEN NEW.document_type_id IS NOT NULL AND NEW.system_id IS NOT (SELECT system_id FROM document_types WHERE id=NEW.document_type_id)
BEGIN SELECT RAISE(ABORT,'cross-system document type id'); END;

CREATE TRIGGER documents_document_type_id_update BEFORE UPDATE OF system_id,document_type_id ON documents
WHEN NEW.document_type_id IS NOT NULL AND NEW.system_id IS NOT (SELECT system_id FROM document_types WHERE id=NEW.document_type_id)
BEGIN SELECT RAISE(ABORT,'cross-system document type id'); END;

CREATE TRIGGER documents_storage_path_id_insert BEFORE INSERT ON documents
WHEN NEW.storage_path_id IS NOT NULL AND NEW.system_id IS NOT (SELECT system_id FROM storage_paths WHERE id=NEW.storage_path_id)
BEGIN SELECT RAISE(ABORT,'cross-system storage path id'); END;

CREATE TRIGGER documents_storage_path_id_update BEFORE UPDATE OF system_id,storage_path_id ON documents
WHEN NEW.storage_path_id IS NOT NULL AND NEW.system_id IS NOT (SELECT system_id FROM storage_paths WHERE id=NEW.storage_path_id)
BEGIN SELECT RAISE(ABORT,'cross-system storage path id'); END;

CREATE TRIGGER documents_previous_version_id_insert BEFORE INSERT ON documents
WHEN NEW.previous_version_id IS NOT NULL AND NEW.system_id IS NOT (SELECT system_id FROM documents WHERE id=NEW.previous_version_id)
BEGIN SELECT RAISE(ABORT,'cross-system previous version id'); END;

CREATE TRIGGER documents_previous_version_id_update BEFORE UPDATE OF system_id,previous_version_id ON documents
WHEN NEW.previous_version_id IS NOT NULL AND NEW.system_id IS NOT (SELECT system_id FROM documents WHERE id=NEW.previous_version_id)
BEGIN SELECT RAISE(ABORT,'cross-system previous version id'); END;

CREATE TRIGGER documents_split_parent_id_insert BEFORE INSERT ON documents
WHEN NEW.split_parent_id IS NOT NULL AND NEW.system_id IS NOT (SELECT system_id FROM documents WHERE id=NEW.split_parent_id)
BEGIN SELECT RAISE(ABORT,'cross-system split parent id'); END;

CREATE TRIGGER documents_split_parent_id_update BEFORE UPDATE OF system_id,split_parent_id ON documents
WHEN NEW.split_parent_id IS NOT NULL AND NEW.system_id IS NOT (SELECT system_id FROM documents WHERE id=NEW.split_parent_id)
BEGIN SELECT RAISE(ABORT,'cross-system split parent id'); END;

CREATE TRIGGER documents_email_parent_id_insert BEFORE INSERT ON documents
WHEN NEW.email_parent_id IS NOT NULL AND NEW.system_id IS NOT (SELECT system_id FROM documents WHERE id=NEW.email_parent_id)
BEGIN SELECT RAISE(ABORT,'cross-system email parent id'); END;

CREATE TRIGGER documents_email_parent_id_update BEFORE UPDATE OF system_id,email_parent_id ON documents
WHEN NEW.email_parent_id IS NOT NULL AND NEW.system_id IS NOT (SELECT system_id FROM documents WHERE id=NEW.email_parent_id)
BEGIN SELECT RAISE(ABORT,'cross-system email parent id'); END;

CREATE TRIGGER documents_split_origin_insert BEFORE INSERT ON documents
WHEN NEW.split_origin_id<>0 AND EXISTS (SELECT 1 FROM documents WHERE id=NEW.split_origin_id AND system_id<>NEW.system_id)
BEGIN SELECT RAISE(ABORT,'cross-system split origin'); END;

CREATE TRIGGER documents_split_origin_update BEFORE UPDATE OF system_id,split_origin_id ON documents
WHEN NEW.split_origin_id<>0 AND EXISTS (SELECT 1 FROM documents WHERE id=NEW.split_origin_id AND system_id<>NEW.system_id)
BEGIN SELECT RAISE(ABORT,'cross-system split origin'); END;

CREATE TRIGGER tags_parent_insert BEFORE INSERT ON tags
WHEN NEW.parent_id IS NOT NULL AND NEW.system_id IS NOT (SELECT system_id FROM tags WHERE id=NEW.parent_id)
BEGIN SELECT RAISE(ABORT,'cross-system parent'); END;

CREATE TRIGGER tags_parent_update BEFORE UPDATE OF system_id,parent_id ON tags
WHEN NEW.parent_id IS NOT NULL AND NEW.system_id IS NOT (SELECT system_id FROM tags WHERE id=NEW.parent_id)
BEGIN SELECT RAISE(ABORT,'cross-system parent'); END;

CREATE TRIGGER document_tags_reference_insert BEFORE INSERT ON document_tags
WHEN NEW.tag_id IS NOT NULL AND (SELECT system_id FROM documents WHERE id=NEW.document_id) IS NOT (SELECT system_id FROM tags WHERE id=NEW.tag_id)
BEGIN SELECT RAISE(ABORT,'cross-system reference'); END;

CREATE TRIGGER document_tags_reference_update BEFORE UPDATE OF document_id,tag_id ON document_tags
WHEN NEW.tag_id IS NOT NULL AND (SELECT system_id FROM documents WHERE id=NEW.document_id) IS NOT (SELECT system_id FROM tags WHERE id=NEW.tag_id)
BEGIN SELECT RAISE(ABORT,'cross-system reference'); END;

CREATE TRIGGER document_correspondents_reference_insert BEFORE INSERT ON document_correspondents
WHEN NEW.correspondent_id IS NOT NULL AND (SELECT system_id FROM documents WHERE id=NEW.document_id) IS NOT (SELECT system_id FROM correspondents WHERE id=NEW.correspondent_id)
BEGIN SELECT RAISE(ABORT,'cross-system reference'); END;

CREATE TRIGGER document_correspondents_reference_update BEFORE UPDATE OF document_id,correspondent_id ON document_correspondents
WHEN NEW.correspondent_id IS NOT NULL AND (SELECT system_id FROM documents WHERE id=NEW.document_id) IS NOT (SELECT system_id FROM correspondents WHERE id=NEW.correspondent_id)
BEGIN SELECT RAISE(ABORT,'cross-system reference'); END;

CREATE TRIGGER document_custom_field_values_reference_insert BEFORE INSERT ON document_custom_field_values
WHEN NEW.field_id IS NOT NULL AND (SELECT system_id FROM documents WHERE id=NEW.document_id) IS NOT (SELECT system_id FROM custom_fields WHERE id=NEW.field_id)
BEGIN SELECT RAISE(ABORT,'cross-system reference'); END;

CREATE TRIGGER document_custom_field_values_reference_update BEFORE UPDATE OF document_id,field_id ON document_custom_field_values
WHEN NEW.field_id IS NOT NULL AND (SELECT system_id FROM documents WHERE id=NEW.document_id) IS NOT (SELECT system_id FROM custom_fields WHERE id=NEW.field_id)
BEGIN SELECT RAISE(ABORT,'cross-system reference'); END;

CREATE TRIGGER document_sources_reference_insert BEFORE INSERT ON document_sources
WHEN NEW.email_account_id IS NOT NULL AND (SELECT system_id FROM documents WHERE id=NEW.document_id) IS NOT (SELECT system_id FROM email_accounts WHERE id=NEW.email_account_id)
BEGIN SELECT RAISE(ABORT,'cross-system reference'); END;

CREATE TRIGGER document_sources_reference_update BEFORE UPDATE OF document_id,email_account_id ON document_sources
WHEN NEW.email_account_id IS NOT NULL AND (SELECT system_id FROM documents WHERE id=NEW.document_id) IS NOT (SELECT system_id FROM email_accounts WHERE id=NEW.email_account_id)
BEGIN SELECT RAISE(ABORT,'cross-system reference'); END;

CREATE TRIGGER approval_runs_definition_insert BEFORE INSERT ON approval_runs
WHEN NEW.system_id IS NOT (SELECT system_id FROM approval_defs WHERE id=NEW.def_id)
BEGIN SELECT RAISE(ABORT,'cross-system definition'); END;

CREATE TRIGGER approval_runs_definition_update BEFORE UPDATE OF system_id,def_id ON approval_runs
WHEN NEW.system_id IS NOT (SELECT system_id FROM approval_defs WHERE id=NEW.def_id)
BEGIN SELECT RAISE(ABORT,'cross-system definition'); END;

CREATE TRIGGER approval_runs_document_insert BEFORE INSERT ON approval_runs
WHEN NEW.doc_id IS NOT NULL AND NEW.system_id IS NOT (SELECT system_id FROM documents WHERE id=NEW.doc_id)
BEGIN SELECT RAISE(ABORT,'cross-system document'); END;

CREATE TRIGGER approval_runs_document_update BEFORE UPDATE OF system_id,doc_id ON approval_runs
WHEN NEW.doc_id IS NOT NULL AND NEW.system_id IS NOT (SELECT system_id FROM documents WHERE id=NEW.doc_id)
BEGIN SELECT RAISE(ABORT,'cross-system document'); END;

CREATE TRIGGER decryption_passwords_last_document_insert BEFORE INSERT ON decryption_passwords
WHEN NEW.last_used_doc_id IS NOT NULL AND EXISTS (SELECT 1 FROM documents WHERE id=NEW.last_used_doc_id AND system_id<>NEW.system_id)
BEGIN SELECT RAISE(ABORT,'cross-system last document'); END;

CREATE TRIGGER decryption_passwords_last_document_update BEFORE UPDATE OF system_id,last_used_doc_id ON decryption_passwords
WHEN NEW.last_used_doc_id IS NOT NULL AND EXISTS (SELECT 1 FROM documents WHERE id=NEW.last_used_doc_id AND system_id<>NEW.system_id)
BEGIN SELECT RAISE(ABORT,'cross-system last document'); END;

CREATE TRIGGER jobs_document_insert BEFORE INSERT ON jobs
WHEN NEW.doc_id IS NOT NULL AND NEW.doc_id<>0 AND NEW.system_id IS NOT (SELECT system_id FROM documents WHERE id=NEW.doc_id)
BEGIN SELECT RAISE(ABORT,'cross-system document'); END;

CREATE TRIGGER jobs_document_update BEFORE UPDATE OF system_id,doc_id ON jobs
WHEN NEW.doc_id IS NOT NULL AND NEW.doc_id<>0 AND NEW.system_id IS NOT (SELECT system_id FROM documents WHERE id=NEW.doc_id)
BEGIN SELECT RAISE(ABORT,'cross-system document'); END;

CREATE TRIGGER share_links_documents_insert BEFORE INSERT ON share_links
WHEN EXISTS (SELECT 1 FROM json_each(NEW.doc_ids_json) j JOIN documents d ON d.id=j.value WHERE d.system_id<>NEW.system_id)
BEGIN SELECT RAISE(ABORT,'cross-system documents'); END;

CREATE TRIGGER share_links_documents_update BEFORE UPDATE OF system_id,doc_ids_json ON share_links
WHEN EXISTS (SELECT 1 FROM json_each(NEW.doc_ids_json) j JOIN documents d ON d.id=j.value WHERE d.system_id<>NEW.system_id)
BEGIN SELECT RAISE(ABORT,'cross-system documents'); END;

CREATE TRIGGER automation_triggers_filter_tag_id_insert BEFORE INSERT ON automation_triggers
WHEN NEW.filter_tag_id IS NOT NULL AND NEW.filter_tag_id<>0 AND (SELECT system_id FROM automations WHERE id=NEW.automation_id) IS NOT (SELECT system_id FROM tags WHERE id=NEW.filter_tag_id)
BEGIN SELECT RAISE(ABORT,'cross-system filter tag id'); END;

CREATE TRIGGER automation_triggers_filter_tag_id_update BEFORE UPDATE OF automation_id,filter_tag_id ON automation_triggers
WHEN NEW.filter_tag_id IS NOT NULL AND NEW.filter_tag_id<>0 AND (SELECT system_id FROM automations WHERE id=NEW.automation_id) IS NOT (SELECT system_id FROM tags WHERE id=NEW.filter_tag_id)
BEGIN SELECT RAISE(ABORT,'cross-system filter tag id'); END;

CREATE TRIGGER automation_triggers_filter_corr_id_insert BEFORE INSERT ON automation_triggers
WHEN NEW.filter_corr_id IS NOT NULL AND NEW.filter_corr_id<>0 AND (SELECT system_id FROM automations WHERE id=NEW.automation_id) IS NOT (SELECT system_id FROM correspondents WHERE id=NEW.filter_corr_id)
BEGIN SELECT RAISE(ABORT,'cross-system filter corr id'); END;

CREATE TRIGGER automation_triggers_filter_corr_id_update BEFORE UPDATE OF automation_id,filter_corr_id ON automation_triggers
WHEN NEW.filter_corr_id IS NOT NULL AND NEW.filter_corr_id<>0 AND (SELECT system_id FROM automations WHERE id=NEW.automation_id) IS NOT (SELECT system_id FROM correspondents WHERE id=NEW.filter_corr_id)
BEGIN SELECT RAISE(ABORT,'cross-system filter corr id'); END;

CREATE TRIGGER automation_triggers_filter_doctype_id_insert BEFORE INSERT ON automation_triggers
WHEN NEW.filter_doctype_id IS NOT NULL AND NEW.filter_doctype_id<>0 AND (SELECT system_id FROM automations WHERE id=NEW.automation_id) IS NOT (SELECT system_id FROM document_types WHERE id=NEW.filter_doctype_id)
BEGIN SELECT RAISE(ABORT,'cross-system filter doctype id'); END;

CREATE TRIGGER automation_triggers_filter_doctype_id_update BEFORE UPDATE OF automation_id,filter_doctype_id ON automation_triggers
WHEN NEW.filter_doctype_id IS NOT NULL AND NEW.filter_doctype_id<>0 AND (SELECT system_id FROM automations WHERE id=NEW.automation_id) IS NOT (SELECT system_id FROM document_types WHERE id=NEW.filter_doctype_id)
BEGIN SELECT RAISE(ABORT,'cross-system filter doctype id'); END;

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
