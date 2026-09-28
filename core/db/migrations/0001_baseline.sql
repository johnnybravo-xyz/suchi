-- Suchi stable-v1 declarative baseline.
-- This file is immutable after the v0.1.0 release; add numbered migrations instead.
-- It describes the final schema directly and contains no pre-release rebuild history.

-- ---- tables ----

CREATE TABLE "api_tokens" (
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

CREATE TABLE "approval_defs" (
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

CREATE TABLE "approval_runs" (
    id               INTEGER PRIMARY KEY AUTOINCREMENT,
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
, revision INTEGER NOT NULL DEFAULT 0) STRICT;

CREATE TABLE "approval_tasks" (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    run_id INTEGER NOT NULL REFERENCES approval_runs(id) ON DELETE CASCADE,
    state_key TEXT NOT NULL,
    assignee TEXT NOT NULL,
    prompt TEXT NOT NULL,
    choices_json TEXT NOT NULL,
    status TEXT NOT NULL,
    deadline_at INTEGER,
    resolved_choice TEXT,
    resolved_by TEXT,
    resolved_at INTEGER,
    created_at INTEGER NOT NULL,
    state_revision INTEGER NOT NULL DEFAULT -1
, resolution_principal_json TEXT) STRICT;

CREATE TABLE "approval_transitions" (
  id            INTEGER PRIMARY KEY,
  run_id        INTEGER NOT NULL REFERENCES "approval_runs"(id) ON DELETE CASCADE,
  from_state    TEXT NOT NULL,
  to_state      TEXT NOT NULL,
  trigger       TEXT NOT NULL,              -- system|timeout|approve|reject|<custom>
  actor         TEXT,                       -- "user:5" | "system" | NULL
  payload_json  TEXT NOT NULL DEFAULT '{}',
  occurred_at   INTEGER NOT NULL
) STRICT;

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

CREATE TABLE automation_actions (
  id             INTEGER PRIMARY KEY,
  automation_id  INTEGER NOT NULL REFERENCES automations(id) ON DELETE CASCADE,
  order_index  INTEGER NOT NULL DEFAULT 0,
  -- assign_title | assign_tags | assign_correspondent | assign_document_type
  -- | assign_jd_category | assign_storage_path | assign_owner | assign_custom_field
  -- | discard
  -- | remove_tags | remove_correspondents | remove_document_type
  -- | remove_storage_path | remove_custom_field
  kind         TEXT NOT NULL,
  -- Params live in a small JSON blob. Shape depends on kind:
  --   assign_title            {"template": "{{correspondent}} — {{title}}"}
  --   assign_tags             {"tag_ids": [1,2,3]}
  --   assign_correspondent    {"correspondent_id": 5}
  --   assign_document_type    {"document_type_id": 7}
  --   assign_jd_category      {"jd_category_id": 8}
  --   assign_storage_path     {"storage_path_id": 3}
  --   assign_owner            {"owner_id": 2}
  --   assign_custom_field     {"field_id": 4, "value": "..."}
  --   discard                 {}
  --   remove_tags             {"tag_ids": [1,2]}
  --   remove_correspondents   {"correspondent_ids": [5]}
  --   remove_document_type    {}
  --   remove_storage_path     {}
  --   remove_custom_field     {"field_id": 4}
  params_json  TEXT NOT NULL DEFAULT '{}',
  created_at   INTEGER NOT NULL
) STRICT;

CREATE TABLE automation_triggers (
  id             INTEGER PRIMARY KEY,
  automation_id  INTEGER NOT NULL REFERENCES automations(id) ON DELETE CASCADE,
  -- consumption | document_added | document_updated
  type           TEXT NOT NULL,
  -- Optional filters. NULL = "any". Every non-null field must match.
  filter_path       TEXT,   -- glob against source path (consumption trigger)
  filter_filename   TEXT,   -- glob against filename (consumption trigger)
  filter_mailrule_id INTEGER, -- consumption via mail-intake rule id
  filter_tag_id      INTEGER, -- doc carries this tag (added/updated)
  filter_corr_id     INTEGER, -- doc has this correspondent (added/updated)
  filter_doctype_id  INTEGER, -- doc has this document_type (added/updated)
  filter_title_re     TEXT,   -- regex against documents.title
  filter_content_re  TEXT,   -- regex against documents.content
  filter_email_from TEXT,
  filter_email_subject TEXT,
  filter_email_folder TEXT,
  filter_email_has_attachment INTEGER,
  created_at   INTEGER NOT NULL
) STRICT;

CREATE TABLE "automations" (
    id          INTEGER PRIMARY KEY,
    name        TEXT NOT NULL,
    order_index INTEGER NOT NULL DEFAULT 0,
    enabled     INTEGER NOT NULL DEFAULT 1,
    created_at  INTEGER NOT NULL,
    updated_at  INTEGER NOT NULL,
    preset_slug TEXT,
    system_id INTEGER NOT NULL REFERENCES jd_systems(id), suspended INTEGER NOT NULL DEFAULT 0
    CHECK (suspended IN (0, 1)),
    UNIQUE(system_id,name)
) STRICT;

CREATE TABLE "correspondents" (
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

CREATE TABLE "custom_fields" (
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

CREATE TABLE "decryption_passwords" (
    id            INTEGER PRIMARY KEY,
    owner_id      INTEGER NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    ciphertext    BLOB NOT NULL,
    label         TEXT,
    created_at    INTEGER NOT NULL,
    last_used_at  INTEGER,
    last_used_doc_id INTEGER,
    system_id INTEGER NOT NULL REFERENCES jd_systems(id)
) STRICT;

CREATE TABLE document_correspondents (
    document_id      INTEGER NOT NULL REFERENCES documents(id) ON DELETE CASCADE,
    correspondent_id INTEGER NOT NULL REFERENCES correspondents(id) ON DELETE CASCADE,
    role             TEXT NOT NULL CHECK (role IN ('sender','recipient','cc','other')),
    position         INTEGER NOT NULL DEFAULT 0,
    PRIMARY KEY (document_id, correspondent_id, role)
) STRICT;

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

CREATE TABLE "document_intelligence" (
 id INTEGER PRIMARY KEY AUTOINCREMENT,
 document_id INTEGER NOT NULL REFERENCES documents(id) ON DELETE CASCADE,
 intelligence_type TEXT NOT NULL,
 role TEXT NOT NULL DEFAULT '',
 value_json TEXT NOT NULL,
 sort_value TEXT NOT NULL DEFAULT '',
 raw_text TEXT NOT NULL DEFAULT '',
 evidence_text TEXT NOT NULL,
 evidence_start INTEGER,
 confidence REAL NOT NULL CHECK (confidence>=0.0 AND confidence<=1.0),
 status TEXT NOT NULL DEFAULT 'pending' CHECK (status IN ('pending','accepted','rejected')),
 extractor TEXT NOT NULL,
 source_blob TEXT NOT NULL DEFAULT '',
 extraction_version INTEGER NOT NULL,
 reviewed_by INTEGER REFERENCES users(id) ON DELETE SET NULL,
 reviewed_at INTEGER,
 created_at INTEGER NOT NULL,
 updated_at INTEGER NOT NULL,
 source_revision INTEGER,
 gate_reason TEXT NOT NULL DEFAULT '',
 gate_policy_version TEXT NOT NULL DEFAULT '',
 UNIQUE(document_id,intelligence_type,role,value_json,evidence_text,extractor)
) STRICT;

CREATE TABLE document_sources (
    id               INTEGER PRIMARY KEY,
    document_id      INTEGER NOT NULL REFERENCES documents(id) ON DELETE CASCADE,
    kind             TEXT NOT NULL CHECK (kind IN (
                         'upload', 'api', 'mailbox', 'watched_folder',
                         'import'
                     )),
    label            TEXT NOT NULL DEFAULT '',
    detail           TEXT NOT NULL DEFAULT '',
    observed_at      INTEGER NOT NULL,
    email_account_id INTEGER REFERENCES email_accounts(id) ON DELETE SET NULL,
    CHECK (email_account_id IS NULL OR kind = 'mailbox')
) STRICT;

CREATE TABLE document_tags (
    document_id  INTEGER NOT NULL REFERENCES documents(id) ON DELETE CASCADE,
    tag_id       INTEGER NOT NULL REFERENCES tags(id) ON DELETE CASCADE, classifier_owned INTEGER NOT NULL DEFAULT 0
    CHECK (classifier_owned IN (0, 1)),
    PRIMARY KEY (document_id, tag_id)
) STRICT;

CREATE TABLE "document_types" (
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
    system_id INTEGER NOT NULL REFERENCES jd_systems(id), source_revision INTEGER NOT NULL DEFAULT 0, title_revision INTEGER NOT NULL DEFAULT 0, correspondent_revision INTEGER NOT NULL DEFAULT 0, document_type_revision INTEGER NOT NULL DEFAULT 0, category_revision INTEGER NOT NULL DEFAULT 0, tags_revision INTEGER NOT NULL DEFAULT 0, language_revision INTEGER NOT NULL DEFAULT 0,
    FOREIGN KEY(jd_category_id,system_id) REFERENCES jd_categories(id,system_id)
) STRICT;

CREATE VIRTUAL TABLE documents_fts USING fts5 (
    title,
    content,
    content='documents',
    content_rowid='id',
    tokenize='porter unicode61 remove_diacritics 2'
);

CREATE TABLE "email_accounts" (
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

CREATE TABLE group_members (
  group_id   INTEGER NOT NULL REFERENCES groups(id) ON DELETE CASCADE,
  user_id    INTEGER NOT NULL REFERENCES users(id)  ON DELETE CASCADE,
  created_at INTEGER NOT NULL,
  PRIMARY KEY (group_id, user_id)
) STRICT;

CREATE TABLE groups (
  id           INTEGER PRIMARY KEY,
  name         TEXT NOT NULL UNIQUE,
  description  TEXT NOT NULL DEFAULT '',
  created_at   INTEGER NOT NULL,
  updated_at   INTEGER NOT NULL
) STRICT;

CREATE TABLE "jd_areas" (
    code_start   INTEGER NOT NULL,
    code_end     INTEGER NOT NULL,
    name         TEXT NOT NULL,
    description  TEXT,
    position     INTEGER NOT NULL,
    system_id INTEGER NOT NULL REFERENCES jd_systems(id),
    PRIMARY KEY(system_id,code_start)
) STRICT;

CREATE TABLE "jd_categories" (
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

CREATE TABLE jd_system_members (
    system_id INTEGER NOT NULL REFERENCES jd_systems(id),
    user_id INTEGER NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    created_at INTEGER NOT NULL,
    PRIMARY KEY(system_id,user_id)
) STRICT;

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

CREATE TABLE "jobs" (
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

CREATE TABLE "mobile_pairings" (
    user_id    INTEGER PRIMARY KEY REFERENCES users(id) ON DELETE CASCADE,
    code_hash  TEXT NOT NULL UNIQUE CHECK (length(code_hash) = 64),
    name       TEXT NOT NULL CHECK (length(name) BETWEEN 1 AND 64),
    expires_at INTEGER NOT NULL,
    system_id INTEGER NOT NULL REFERENCES jd_systems(id)
) STRICT;

CREATE TABLE notes (
    id           INTEGER PRIMARY KEY,
    document_id  INTEGER NOT NULL REFERENCES documents(id) ON DELETE CASCADE,
    user_id      INTEGER REFERENCES users(id) ON DELETE SET NULL,
    note         TEXT NOT NULL,
    created_at   INTEGER NOT NULL
) STRICT;

CREATE TABLE object_acls (
  id             INTEGER PRIMARY KEY,
  object_kind    TEXT    NOT NULL CHECK (object_kind IN (
      'document', 'tag', 'correspondent', 'document_type', 'storage_path'
  )),
  object_id      INTEGER NOT NULL,
  principal_kind TEXT    NOT NULL CHECK (principal_kind IN ('user', 'group')),
  principal_id   INTEGER NOT NULL,
  perm_bits      INTEGER NOT NULL DEFAULT 0,
  created_at     INTEGER NOT NULL,
  created_by     INTEGER REFERENCES users(id) ON DELETE SET NULL,
  UNIQUE (object_kind, object_id, principal_kind, principal_id)
) STRICT;

CREATE TABLE plugin_kv (
    plugin_name TEXT NOT NULL,
    key         TEXT NOT NULL,
    value_json  TEXT NOT NULL,
    updated_at  INTEGER NOT NULL,
    PRIMARY KEY (plugin_name, key)
) STRICT;

CREATE TABLE render_moves (
    id           INTEGER PRIMARY KEY,
    document_id  INTEGER NOT NULL REFERENCES documents(id) ON DELETE CASCADE,
    prev_path    TEXT NOT NULL,   -- relative to renderDir; "" for the first render of a doc
    new_path     TEXT NOT NULL,   -- relative to renderDir
    state        TEXT NOT NULL CHECK (state IN ('pending','applied','failed')),
    created_at   INTEGER NOT NULL,
    applied_at   INTEGER,
    err          TEXT
, prev_blob TEXT NOT NULL DEFAULT '', new_blob TEXT NOT NULL DEFAULT '') STRICT;

CREATE TABLE "saved_views" (
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

CREATE TABLE schema_lineage (
    singleton INTEGER PRIMARY KEY CHECK (singleton = 1),
    name TEXT NOT NULL
) STRICT;

CREATE TABLE sessions (
    id           TEXT PRIMARY KEY,
    user_id      INTEGER NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    created_at   INTEGER NOT NULL,
    expires_at   INTEGER NOT NULL,
    last_seen_at INTEGER NOT NULL,
    user_agent   TEXT,
    ip           TEXT
) STRICT;

CREATE TABLE settings (
    key        TEXT PRIMARY KEY,
    value_json TEXT NOT NULL,
    updated_at INTEGER NOT NULL
) STRICT;

CREATE TABLE "share_links" (
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

CREATE TABLE "storage_paths" (
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

CREATE TABLE "tags" (
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
    parent_id INTEGER REFERENCES tags(id) ON DELETE SET NULL,
    system_id INTEGER NOT NULL REFERENCES jd_systems(id),
    UNIQUE(system_id,name),
    UNIQUE(system_id,slug)
) STRICT;

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

CREATE TABLE users (
    id           INTEGER PRIMARY KEY,
    email        TEXT NOT NULL UNIQUE,
    display_name TEXT NOT NULL,
    role         TEXT NOT NULL CHECK (role IN ('admin','member')),
    disabled     INTEGER NOT NULL DEFAULT 0,
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

-- ---- initial rows ----

INSERT INTO jd_systems (
    id, code, name, taxonomy, preset_id, preset_version, preset_sha256,
    authoring_json, created_at, updated_at
) VALUES (1, '', 'Archive', 'jd', NULL, NULL, NULL, NULL, unixepoch(), unixepoch());

INSERT INTO schema_lineage(singleton, name) VALUES (1, 'stable-v1');

-- ---- indexes ----

CREATE INDEX api_tokens_user ON api_tokens(system_id, user_id);

CREATE INDEX audit_actor ON audit_events(actor_kind, actor_id);

CREATE INDEX audit_object ON audit_events(object_kind, object_id);

CREATE INDEX audit_system ON audit_events(system_id,id);

CREATE INDEX audit_ts ON audit_events(ts);

CREATE UNIQUE INDEX users_oidc_identity
    ON users(oidc_issuer, oidc_subject) WHERE oidc_issuer IS NOT NULL;

CREATE INDEX dcfv_field ON document_custom_field_values(field_id);

CREATE INDEX decryption_passwords_owner
    ON decryption_passwords(system_id, owner_id, last_used_at DESC);

CREATE INDEX document_correspondents_corr ON document_correspondents(correspondent_id);

CREATE INDEX document_correspondents_doc  ON document_correspondents(document_id);

CREATE INDEX document_sources_document
  ON document_sources(document_id);

CREATE INDEX document_sources_email_account
  ON document_sources(email_account_id)
  WHERE email_account_id IS NOT NULL;

CREATE UNIQUE INDEX document_sources_mailbox_uniq
  ON document_sources(document_id, email_account_id, detail)
  WHERE email_account_id IS NOT NULL;

CREATE INDEX document_tags_tag ON document_tags(tag_id);

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

CREATE INDEX group_members_user ON group_members(user_id);

CREATE INDEX idx_approval_defs_active        ON approval_defs(system_id, active, slug);

CREATE INDEX idx_approval_runs_active        ON approval_runs(system_id, state, deadline_at);

CREATE INDEX idx_approval_runs_doc           ON approval_runs(doc_id);

CREATE INDEX idx_approval_tasks_open ON approval_tasks(status,assignee);

CREATE UNIQUE INDEX idx_approval_tasks_open_state ON approval_tasks(run_id,state_key)
    WHERE status IN ('open','claimed');

CREATE INDEX idx_approval_tasks_run ON approval_tasks(run_id);

CREATE INDEX idx_approval_transitions_run    ON approval_transitions(run_id, occurred_at);

CREATE INDEX idx_automation_actions_aid  ON automation_actions(automation_id, order_index);

CREATE INDEX idx_automation_triggers_aid  ON automation_triggers(automation_id);

CREATE INDEX idx_automation_triggers_type ON automation_triggers(type);

CREATE INDEX idx_automations_enabled ON automations(system_id, enabled, order_index)
  WHERE enabled = 1;

CREATE INDEX idx_automations_preset_slug
  ON automations(system_id, preset_slug) WHERE preset_slug IS NOT NULL;

CREATE INDEX idx_document_intelligence_document ON document_intelligence(document_id,status,intelligence_type);

CREATE INDEX idx_document_intelligence_review ON document_intelligence(status,intelligence_type,sort_value,document_id);

CREATE INDEX idx_email_accounts_enabled ON email_accounts(system_id, enabled);

CREATE INDEX idx_saved_views_owner ON saved_views(system_id, owner_id, position);

CREATE INDEX idx_share_links_creator ON share_links(system_id, created_by, created_at DESC);

CREATE INDEX idx_share_links_expires ON share_links(expires_at) WHERE expires_at IS NOT NULL;

CREATE INDEX jd_system_members_user ON jd_system_members(user_id,system_id);

CREATE INDEX jobs_doc ON jobs(doc_id);

CREATE INDEX jobs_ready ON jobs(next_run_at) WHERE state = 'pending';

CREATE INDEX jobs_state ON jobs(state);

CREATE INDEX jobs_system ON jobs(system_id,id);

CREATE INDEX notes_document ON notes(document_id);

CREATE INDEX object_acls_lookup
  ON object_acls(object_kind, object_id, principal_kind, principal_id);

CREATE INDEX object_acls_principal
  ON object_acls(principal_kind, principal_id, object_kind);

CREATE INDEX render_moves_doc     ON render_moves(document_id, applied_at DESC);

CREATE INDEX render_moves_pending ON render_moves(state, created_at) WHERE state = 'pending';

CREATE INDEX sessions_expiry ON sessions(expires_at);

CREATE INDEX sessions_user ON sessions(user_id);

CREATE INDEX tags_parent ON tags(parent_id) WHERE parent_id IS NOT NULL;

CREATE INDEX upload_idempotency_created_at
    ON upload_idempotency(created_at);

-- ---- triggers ----

CREATE TRIGGER api_tokens_system_immutable BEFORE UPDATE OF system_id ON api_tokens
WHEN NEW.system_id IS NOT OLD.system_id
BEGIN SELECT RAISE(ABORT,'system ownership is immutable'); END;

CREATE TRIGGER api_tokens_system_replace BEFORE INSERT ON api_tokens
WHEN EXISTS (SELECT 1 FROM api_tokens WHERE id=NEW.id AND system_id IS NOT NEW.system_id)
BEGIN SELECT RAISE(ABORT,'system ownership is immutable'); END;

CREATE TRIGGER approval_defs_system_immutable BEFORE UPDATE OF system_id ON approval_defs
WHEN NEW.system_id IS NOT OLD.system_id
BEGIN SELECT RAISE(ABORT,'system ownership is immutable'); END;

CREATE TRIGGER approval_defs_system_replace BEFORE INSERT ON approval_defs
WHEN EXISTS (SELECT 1 FROM approval_defs WHERE id=NEW.id AND system_id IS NOT NEW.system_id)
BEGIN SELECT RAISE(ABORT,'system ownership is immutable'); END;

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

CREATE TRIGGER approval_runs_system_immutable BEFORE UPDATE OF system_id ON approval_runs
WHEN NEW.system_id IS NOT OLD.system_id
BEGIN SELECT RAISE(ABORT,'system ownership is immutable'); END;

CREATE TRIGGER approval_runs_system_replace BEFORE INSERT ON approval_runs
WHEN EXISTS (SELECT 1 FROM approval_runs WHERE id=NEW.id AND system_id IS NOT NEW.system_id)
BEGIN SELECT RAISE(ABORT,'system ownership is immutable'); END;

CREATE TRIGGER audit_events_system_immutable BEFORE UPDATE OF system_id ON audit_events
WHEN NEW.system_id IS NOT OLD.system_id
BEGIN SELECT RAISE(ABORT,'system ownership is immutable'); END;

CREATE TRIGGER audit_events_system_replace BEFORE INSERT ON audit_events
WHEN EXISTS (SELECT 1 FROM audit_events WHERE id=NEW.id AND system_id IS NOT NEW.system_id)
BEGIN SELECT RAISE(ABORT,'system ownership is immutable'); END;

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

CREATE TRIGGER automation_triggers_filter_tag_id_insert BEFORE INSERT ON automation_triggers
WHEN NEW.filter_tag_id IS NOT NULL AND NEW.filter_tag_id<>0 AND (SELECT system_id FROM automations WHERE id=NEW.automation_id) IS NOT (SELECT system_id FROM tags WHERE id=NEW.filter_tag_id)
BEGIN SELECT RAISE(ABORT,'cross-system filter tag id'); END;

CREATE TRIGGER automation_triggers_filter_tag_id_update BEFORE UPDATE OF automation_id,filter_tag_id ON automation_triggers
WHEN NEW.filter_tag_id IS NOT NULL AND NEW.filter_tag_id<>0 AND (SELECT system_id FROM automations WHERE id=NEW.automation_id) IS NOT (SELECT system_id FROM tags WHERE id=NEW.filter_tag_id)
BEGIN SELECT RAISE(ABORT,'cross-system filter tag id'); END;

CREATE TRIGGER automations_system_immutable BEFORE UPDATE OF system_id ON automations
WHEN NEW.system_id IS NOT OLD.system_id
BEGIN SELECT RAISE(ABORT,'system ownership is immutable'); END;

CREATE TRIGGER automations_system_replace BEFORE INSERT ON automations
WHEN EXISTS (SELECT 1 FROM automations WHERE id=NEW.id AND system_id IS NOT NEW.system_id)
BEGIN SELECT RAISE(ABORT,'system ownership is immutable'); END;

CREATE TRIGGER correspondents_system_immutable BEFORE UPDATE OF system_id ON correspondents
WHEN NEW.system_id IS NOT OLD.system_id
BEGIN SELECT RAISE(ABORT,'system ownership is immutable'); END;

CREATE TRIGGER correspondents_system_replace BEFORE INSERT ON correspondents
WHEN EXISTS (SELECT 1 FROM correspondents WHERE id=NEW.id AND system_id IS NOT NEW.system_id)
BEGIN SELECT RAISE(ABORT,'system ownership is immutable'); END;

CREATE TRIGGER custom_fields_system_immutable BEFORE UPDATE OF system_id ON custom_fields
WHEN NEW.system_id IS NOT OLD.system_id
BEGIN SELECT RAISE(ABORT,'system ownership is immutable'); END;

CREATE TRIGGER custom_fields_system_replace BEFORE INSERT ON custom_fields
WHEN EXISTS (SELECT 1 FROM custom_fields WHERE id=NEW.id AND system_id IS NOT NEW.system_id)
BEGIN SELECT RAISE(ABORT,'system ownership is immutable'); END;

CREATE TRIGGER decryption_passwords_last_document_insert BEFORE INSERT ON decryption_passwords
WHEN NEW.last_used_doc_id IS NOT NULL AND EXISTS (SELECT 1 FROM documents WHERE id=NEW.last_used_doc_id AND system_id<>NEW.system_id)
BEGIN SELECT RAISE(ABORT,'cross-system last document'); END;

CREATE TRIGGER decryption_passwords_last_document_update BEFORE UPDATE OF system_id,last_used_doc_id ON decryption_passwords
WHEN NEW.last_used_doc_id IS NOT NULL AND EXISTS (SELECT 1 FROM documents WHERE id=NEW.last_used_doc_id AND system_id<>NEW.system_id)
BEGIN SELECT RAISE(ABORT,'cross-system last document'); END;

CREATE TRIGGER decryption_passwords_system_immutable BEFORE UPDATE OF system_id ON decryption_passwords
WHEN NEW.system_id IS NOT OLD.system_id
BEGIN SELECT RAISE(ABORT,'system ownership is immutable'); END;

CREATE TRIGGER decryption_passwords_system_replace BEFORE INSERT ON decryption_passwords
WHEN EXISTS (SELECT 1 FROM decryption_passwords WHERE id=NEW.id AND system_id IS NOT NEW.system_id)
BEGIN SELECT RAISE(ABORT,'system ownership is immutable'); END;

CREATE TRIGGER document_correspondents_reference_insert BEFORE INSERT ON document_correspondents
WHEN NEW.correspondent_id IS NOT NULL AND (SELECT system_id FROM documents WHERE id=NEW.document_id) IS NOT (SELECT system_id FROM correspondents WHERE id=NEW.correspondent_id)
BEGIN SELECT RAISE(ABORT,'cross-system reference'); END;

CREATE TRIGGER document_correspondents_reference_update BEFORE UPDATE OF document_id,correspondent_id ON document_correspondents
WHEN NEW.correspondent_id IS NOT NULL AND (SELECT system_id FROM documents WHERE id=NEW.document_id) IS NOT (SELECT system_id FROM correspondents WHERE id=NEW.correspondent_id)
BEGIN SELECT RAISE(ABORT,'cross-system reference'); END;

CREATE TRIGGER document_correspondents_revision_delete AFTER DELETE ON document_correspondents
BEGIN UPDATE documents SET correspondent_revision=correspondent_revision+1 WHERE id=OLD.document_id; END;

CREATE TRIGGER document_correspondents_revision_insert AFTER INSERT ON document_correspondents
BEGIN UPDATE documents SET correspondent_revision=correspondent_revision+1 WHERE id=NEW.document_id; END;

CREATE TRIGGER document_correspondents_revision_update AFTER UPDATE ON document_correspondents
BEGIN
 UPDATE documents SET correspondent_revision=correspondent_revision+1 WHERE id=OLD.document_id;
 UPDATE documents SET correspondent_revision=correspondent_revision+1 WHERE id=NEW.document_id AND NEW.document_id<>OLD.document_id;
END;

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

CREATE TRIGGER document_sources_revision_delete AFTER DELETE ON document_sources
BEGIN UPDATE documents SET source_revision=source_revision+1 WHERE id=OLD.document_id; END;

CREATE TRIGGER document_sources_revision_insert AFTER INSERT ON document_sources
BEGIN UPDATE documents SET source_revision=source_revision+1 WHERE id=NEW.document_id; END;

CREATE TRIGGER document_sources_revision_update AFTER UPDATE ON document_sources
BEGIN
 UPDATE documents SET source_revision=source_revision+1 WHERE id=OLD.document_id;
 UPDATE documents SET source_revision=source_revision+1 WHERE id=NEW.document_id AND NEW.document_id<>OLD.document_id;
END;

CREATE TRIGGER document_tags_reference_insert BEFORE INSERT ON document_tags
WHEN NEW.tag_id IS NOT NULL AND (SELECT system_id FROM documents WHERE id=NEW.document_id) IS NOT (SELECT system_id FROM tags WHERE id=NEW.tag_id)
BEGIN SELECT RAISE(ABORT,'cross-system reference'); END;

CREATE TRIGGER document_tags_reference_update BEFORE UPDATE OF document_id,tag_id ON document_tags
WHEN NEW.tag_id IS NOT NULL AND (SELECT system_id FROM documents WHERE id=NEW.document_id) IS NOT (SELECT system_id FROM tags WHERE id=NEW.tag_id)
BEGIN SELECT RAISE(ABORT,'cross-system reference'); END;

CREATE TRIGGER document_tags_revision_delete AFTER DELETE ON document_tags WHEN OLD.classifier_owned=0
BEGIN UPDATE documents SET tags_revision=tags_revision+1 WHERE id=OLD.document_id; END;

CREATE TRIGGER document_tags_revision_insert AFTER INSERT ON document_tags WHEN NEW.classifier_owned=0
BEGIN UPDATE documents SET tags_revision=tags_revision+1 WHERE id=NEW.document_id; END;

CREATE TRIGGER document_tags_revision_update AFTER UPDATE ON document_tags
BEGIN
 UPDATE documents SET tags_revision=tags_revision+1 WHERE id=OLD.document_id AND OLD.classifier_owned=0;
 UPDATE documents SET tags_revision=tags_revision+1 WHERE id=NEW.document_id AND NEW.classifier_owned=0
   AND (OLD.classifier_owned<>0 OR OLD.document_id<>NEW.document_id);
END;

CREATE TRIGGER document_types_system_immutable BEFORE UPDATE OF system_id ON document_types
WHEN NEW.system_id IS NOT OLD.system_id
BEGIN SELECT RAISE(ABORT,'system ownership is immutable'); END;

CREATE TRIGGER document_types_system_replace BEFORE INSERT ON document_types
WHEN EXISTS (SELECT 1 FROM document_types WHERE id=NEW.id AND system_id IS NOT NEW.system_id)
BEGIN SELECT RAISE(ABORT,'system ownership is immutable'); END;

CREATE TRIGGER documents_category_revision AFTER UPDATE OF jd_category_id ON documents
BEGIN UPDATE documents SET category_revision=category_revision+1 WHERE id=NEW.id; END;

CREATE TRIGGER documents_correspondent_id_insert BEFORE INSERT ON documents
WHEN NEW.correspondent_id IS NOT NULL AND NEW.system_id IS NOT (SELECT system_id FROM correspondents WHERE id=NEW.correspondent_id)
BEGIN SELECT RAISE(ABORT,'cross-system correspondent id'); END;

CREATE TRIGGER documents_correspondent_id_update BEFORE UPDATE OF system_id,correspondent_id ON documents
WHEN NEW.correspondent_id IS NOT NULL AND NEW.system_id IS NOT (SELECT system_id FROM correspondents WHERE id=NEW.correspondent_id)
BEGIN SELECT RAISE(ABORT,'cross-system correspondent id'); END;

CREATE TRIGGER documents_correspondent_revision AFTER UPDATE OF correspondent_id ON documents
BEGIN UPDATE documents SET correspondent_revision=correspondent_revision+1 WHERE id=NEW.id; END;

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

CREATE TRIGGER email_accounts_system_immutable BEFORE UPDATE OF system_id ON email_accounts
WHEN NEW.system_id IS NOT OLD.system_id
BEGIN SELECT RAISE(ABORT,'system ownership is immutable'); END;

CREATE TRIGGER email_accounts_system_replace BEFORE INSERT ON email_accounts
WHEN EXISTS (SELECT 1 FROM email_accounts WHERE id=NEW.id AND system_id IS NOT NEW.system_id)
BEGIN SELECT RAISE(ABORT,'system ownership is immutable'); END;

CREATE TRIGGER jd_areas_system_immutable BEFORE UPDATE OF system_id ON jd_areas
WHEN NEW.system_id IS NOT OLD.system_id
BEGIN SELECT RAISE(ABORT,'system ownership is immutable'); END;

CREATE TRIGGER jd_categories_inbox_protected BEFORE UPDATE OF system ON jd_categories
WHEN NEW.system<>1 AND EXISTS (SELECT 1 FROM jd_systems WHERE inbox_category_id=OLD.id)
BEGIN SELECT RAISE(ABORT,'Inbox must remain protected'); END;

CREATE TRIGGER jd_categories_system_immutable BEFORE UPDATE OF system_id ON jd_categories
WHEN NEW.system_id IS NOT OLD.system_id
BEGIN SELECT RAISE(ABORT,'system ownership is immutable'); END;

CREATE TRIGGER jd_categories_system_replace BEFORE INSERT ON jd_categories
WHEN EXISTS (SELECT 1 FROM jd_categories WHERE id=NEW.id AND system_id IS NOT NEW.system_id)
BEGIN SELECT RAISE(ABORT,'system ownership is immutable'); END;

CREATE TRIGGER jd_system_members_identity BEFORE UPDATE OF system_id,user_id ON jd_system_members
WHEN NEW.system_id<>OLD.system_id OR NEW.user_id<>OLD.user_id
BEGIN SELECT RAISE(ABORT,'replace membership with explicit removal and grant'); END;

CREATE TRIGGER jd_system_members_revoke AFTER DELETE ON jd_system_members
BEGIN
    DELETE FROM api_tokens WHERE user_id=OLD.user_id AND system_id=OLD.system_id;
    DELETE FROM mobile_pairings WHERE user_id=OLD.user_id AND system_id=OLD.system_id;
    UPDATE share_links SET revoked_at=COALESCE(revoked_at,unixepoch()) WHERE created_by=OLD.user_id AND system_id=OLD.system_id;
END;

CREATE TRIGGER jd_systems_identity_delete BEFORE DELETE ON jd_systems
BEGIN SELECT RAISE(ABORT,'system identity is permanent'); END;

CREATE TRIGGER jd_systems_identity_insert BEFORE INSERT ON jd_systems
WHEN EXISTS (SELECT 1 FROM jd_systems WHERE id=NEW.id AND code<>'' AND code<>NEW.code)
    OR EXISTS (SELECT 1 FROM jd_systems WHERE code=NEW.code AND id<>NEW.id)
BEGIN SELECT RAISE(ABORT,'system identity is permanent'); END;

CREATE TRIGGER jd_systems_identity_update BEFORE UPDATE OF id,code ON jd_systems
WHEN NEW.id<>OLD.id OR (OLD.code<>'' AND NEW.code<>OLD.code)
BEGIN SELECT RAISE(ABORT,'system identity is immutable'); END;

CREATE TRIGGER jd_systems_inbox_insert BEFORE INSERT ON jd_systems
WHEN NEW.inbox_category_id IS NOT NULL AND NOT EXISTS (
    SELECT 1 FROM jd_categories WHERE id=NEW.inbox_category_id AND system_id=NEW.id AND system=1)
BEGIN SELECT RAISE(ABORT,'Inbox must be a protected category in its system'); END;

CREATE TRIGGER jd_systems_inbox_update BEFORE UPDATE OF inbox_category_id ON jd_systems
WHEN NEW.inbox_category_id IS NOT NULL AND NOT EXISTS (
    SELECT 1 FROM jd_categories WHERE id=NEW.inbox_category_id AND system_id=NEW.id AND system=1)
BEGIN SELECT RAISE(ABORT,'Inbox must be a protected category in its system'); END;

CREATE TRIGGER jobs_document_insert BEFORE INSERT ON jobs
WHEN NEW.doc_id IS NOT NULL AND NEW.doc_id<>0 AND NEW.system_id IS NOT (SELECT system_id FROM documents WHERE id=NEW.doc_id)
BEGIN SELECT RAISE(ABORT,'cross-system document'); END;

CREATE TRIGGER jobs_document_update BEFORE UPDATE OF system_id,doc_id ON jobs
WHEN NEW.doc_id IS NOT NULL AND NEW.doc_id<>0 AND NEW.system_id IS NOT (SELECT system_id FROM documents WHERE id=NEW.doc_id)
BEGIN SELECT RAISE(ABORT,'cross-system document'); END;

CREATE TRIGGER jobs_system_immutable BEFORE UPDATE OF system_id ON jobs
WHEN NEW.system_id IS NOT OLD.system_id
BEGIN SELECT RAISE(ABORT,'system ownership is immutable'); END;

CREATE TRIGGER jobs_system_replace BEFORE INSERT ON jobs
WHEN EXISTS (SELECT 1 FROM jobs WHERE id=NEW.id AND system_id IS NOT NEW.system_id)
BEGIN SELECT RAISE(ABORT,'system ownership is immutable'); END;

CREATE TRIGGER mobile_pairings_system_immutable BEFORE UPDATE OF system_id ON mobile_pairings
WHEN NEW.system_id IS NOT OLD.system_id
BEGIN SELECT RAISE(ABORT,'system ownership is immutable'); END;

CREATE TRIGGER mobile_pairings_system_replace BEFORE INSERT ON mobile_pairings
WHEN EXISTS (SELECT 1 FROM mobile_pairings WHERE user_id=NEW.user_id AND system_id IS NOT NEW.system_id)
BEGIN SELECT RAISE(ABORT,'system ownership is immutable'); END;

CREATE TRIGGER saved_views_system_immutable BEFORE UPDATE OF system_id ON saved_views
WHEN NEW.system_id IS NOT OLD.system_id
BEGIN SELECT RAISE(ABORT,'system ownership is immutable'); END;

CREATE TRIGGER saved_views_system_replace BEFORE INSERT ON saved_views
WHEN EXISTS (SELECT 1 FROM saved_views WHERE id=NEW.id AND system_id IS NOT NEW.system_id)
BEGIN SELECT RAISE(ABORT,'system ownership is immutable'); END;

CREATE TRIGGER share_links_documents_insert BEFORE INSERT ON share_links
WHEN EXISTS (SELECT 1 FROM json_each(NEW.doc_ids_json) j JOIN documents d ON d.id=j.value WHERE d.system_id<>NEW.system_id)
BEGIN SELECT RAISE(ABORT,'cross-system documents'); END;

CREATE TRIGGER share_links_documents_update BEFORE UPDATE OF system_id,doc_ids_json ON share_links
WHEN EXISTS (SELECT 1 FROM json_each(NEW.doc_ids_json) j JOIN documents d ON d.id=j.value WHERE d.system_id<>NEW.system_id)
BEGIN SELECT RAISE(ABORT,'cross-system documents'); END;

CREATE TRIGGER share_links_system_immutable BEFORE UPDATE OF system_id ON share_links
WHEN NEW.system_id IS NOT OLD.system_id
BEGIN SELECT RAISE(ABORT,'system ownership is immutable'); END;

CREATE TRIGGER share_links_system_replace BEFORE INSERT ON share_links
WHEN EXISTS (SELECT 1 FROM share_links WHERE id=NEW.id AND system_id IS NOT NEW.system_id)
BEGIN SELECT RAISE(ABORT,'system ownership is immutable'); END;

CREATE TRIGGER storage_paths_system_immutable BEFORE UPDATE OF system_id ON storage_paths
WHEN NEW.system_id IS NOT OLD.system_id
BEGIN SELECT RAISE(ABORT,'system ownership is immutable'); END;

CREATE TRIGGER storage_paths_system_replace BEFORE INSERT ON storage_paths
WHEN EXISTS (SELECT 1 FROM storage_paths WHERE id=NEW.id AND system_id IS NOT NEW.system_id)
BEGIN SELECT RAISE(ABORT,'system ownership is immutable'); END;

CREATE TRIGGER tags_parent_insert BEFORE INSERT ON tags
WHEN NEW.parent_id IS NOT NULL AND NEW.system_id IS NOT (SELECT system_id FROM tags WHERE id=NEW.parent_id)
BEGIN SELECT RAISE(ABORT,'cross-system parent'); END;

CREATE TRIGGER tags_parent_update BEFORE UPDATE OF system_id,parent_id ON tags
WHEN NEW.parent_id IS NOT NULL AND NEW.system_id IS NOT (SELECT system_id FROM tags WHERE id=NEW.parent_id)
BEGIN SELECT RAISE(ABORT,'cross-system parent'); END;

CREATE TRIGGER tags_system_immutable BEFORE UPDATE OF system_id ON tags
WHEN NEW.system_id IS NOT OLD.system_id
BEGIN SELECT RAISE(ABORT,'system ownership is immutable'); END;

CREATE TRIGGER tags_system_replace BEFORE INSERT ON tags
WHEN EXISTS (SELECT 1 FROM tags WHERE id=NEW.id AND system_id IS NOT NEW.system_id)
BEGIN SELECT RAISE(ABORT,'system ownership is immutable'); END;

CREATE TRIGGER users_default_system_demotion AFTER UPDATE OF role ON users
WHEN OLD.role='admin' AND NEW.role='member' AND EXISTS (SELECT 1 FROM jd_systems WHERE id=1 AND code='')
BEGIN INSERT OR IGNORE INTO jd_system_members(system_id,user_id,created_at) VALUES(1,NEW.id,NEW.updated_at); END;

CREATE TRIGGER users_default_system_insert AFTER INSERT ON users
WHEN NEW.role='member' AND EXISTS (SELECT 1 FROM jd_systems WHERE id=1 AND code='')
BEGIN INSERT INTO jd_system_members(system_id,user_id,created_at) VALUES(1,NEW.id,NEW.created_at); END;

PRAGMA user_version = 1;
