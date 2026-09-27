-- suchi baseline schema.
--
-- Folded from all pre-v0.1 migrations. After the v0.1 tag, this file is
-- immutable and every schema change gets a new numbered migration.


-- ---- tables ----

CREATE TABLE api_tokens (
    id           INTEGER PRIMARY KEY,
    user_id      INTEGER NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    name         TEXT NOT NULL,
    token_hash   TEXT NOT NULL UNIQUE,
    scopes       TEXT NOT NULL,
    created_at   INTEGER NOT NULL,
    last_used_at INTEGER,
    revoked_at   INTEGER
) STRICT;

CREATE TABLE "approval_defs" (
  id             INTEGER PRIMARY KEY,
  slug           TEXT NOT NULL,
  version        INTEGER NOT NULL,
  spec_json      TEXT NOT NULL,             -- {states:[{key,kind,assignee,timeout_sec,on:{event:next}}], start}
  active         INTEGER NOT NULL DEFAULT 1,
  created_at     INTEGER NOT NULL,
  created_by     INTEGER REFERENCES users(id),
  UNIQUE(slug, version)
) STRICT;

CREATE TABLE "approval_runs" (
  id               INTEGER PRIMARY KEY,
  def_id           INTEGER NOT NULL REFERENCES "approval_defs"(id),
  doc_id           INTEGER REFERENCES documents(id),
  state            TEXT NOT NULL,           -- running|done|failed|cancelled
  current_state    TEXT NOT NULL,           -- key into spec_json.states
  vars_json        TEXT NOT NULL DEFAULT '{}',
  state_entered_at INTEGER NOT NULL,
  deadline_at      INTEGER,                 -- unix epoch; NULL = no timeout
  started_by       INTEGER REFERENCES users(id),
  started_at       INTEGER NOT NULL,
  ended_at         INTEGER
) STRICT;

CREATE TABLE "approval_tasks" (
  id              INTEGER PRIMARY KEY,
  run_id          INTEGER NOT NULL REFERENCES "approval_runs"(id) ON DELETE CASCADE,
  state_key       TEXT NOT NULL,            -- state that spawned this task
  assignee        TEXT NOT NULL,            -- "user:5" | "role:finance"
  prompt          TEXT NOT NULL,
  choices_json    TEXT NOT NULL,            -- ["approve","reject"]
  status          TEXT NOT NULL,            -- open|claimed|resolved|expired
  deadline_at     INTEGER,
  resolved_choice TEXT,
  resolved_by     TEXT,
  resolved_at     INTEGER,
  created_at      INTEGER NOT NULL
) STRICT;

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

CREATE TABLE audit_events (
    id           INTEGER PRIMARY KEY,
    ts           INTEGER NOT NULL,
    actor_kind   TEXT NOT NULL,          -- 'user' | 'token' | 'system'
    actor_id     INTEGER,                -- user_id or api_tokens.id; NULL for system
    action       TEXT NOT NULL,          -- e.g. 'document.update', 'auth.login'
    object_kind  TEXT NOT NULL,
    object_id    INTEGER,
    before_json  TEXT,
    after_json   TEXT,
    request_id   TEXT
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

CREATE TABLE decryption_passwords (
    id            INTEGER PRIMARY KEY,
    owner_id      INTEGER NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    ciphertext    BLOB NOT NULL,   -- AES-256-GCM seal of the password bytes
    label         TEXT,             -- optional operator-visible name ("BofA 2024")
    created_at    INTEGER NOT NULL,
    last_used_at  INTEGER,
    last_used_doc_id INTEGER
) STRICT;

CREATE TABLE email_accounts (
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
    mark_seen         INTEGER NOT NULL DEFAULT 0
);

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

CREATE TABLE document_tags (
    document_id  INTEGER NOT NULL REFERENCES documents(id) ON DELETE CASCADE,
    tag_id       INTEGER NOT NULL REFERENCES tags(id) ON DELETE CASCADE,
    PRIMARY KEY (document_id, tag_id)
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

CREATE TABLE documents (
    id             INTEGER PRIMARY KEY,
    owner_id       INTEGER NOT NULL REFERENCES users(id),
    -- content hash of the ingested bytes; dedup key.
    original_blob  TEXT NOT NULL,
    original_size  INTEGER NOT NULL,
    -- OCR-rewritten PDF; nullable; regenerable in principle.
    archive_blob   TEXT,
    archive_size   INTEGER,
    title          TEXT NOT NULL DEFAULT '',
    -- JD taxonomy: NOT NULL from day one. Flat mode = one system inbox
    -- category, everything lives there. The setting jd_inbox_category_id
    -- points to it and is seeded by the first-boot Inbox baseline.
    jd_category_id INTEGER NOT NULL REFERENCES jd_categories(id),
    created_at     INTEGER NOT NULL,
    updated_at     INTEGER NOT NULL,
    trashed_at     INTEGER
, content              TEXT, mime_type            TEXT, correspondent_id     INTEGER REFERENCES correspondents(id) ON DELETE SET NULL, document_type_id     INTEGER REFERENCES document_types(id) ON DELETE SET NULL, storage_path_id      INTEGER REFERENCES storage_paths(id) ON DELETE SET NULL, added_at             INTEGER, legacy_id            INTEGER, archive_serial_number INTEGER, previous_version_id INTEGER
    REFERENCES documents(id) ON DELETE SET NULL, split_parent_id INTEGER
    REFERENCES documents(id) ON DELETE SET NULL, split_index INTEGER, encryption_state TEXT
    CHECK (encryption_state IN ('encrypted', 'decrypted')), decrypted_blob TEXT, decrypted_size INTEGER, email_parent_id  INTEGER
    REFERENCES documents(id) ON DELETE SET NULL, email_message_id TEXT, sensitivity TEXT, thumb_sha TEXT, pipeline_version_ocr     INTEGER NOT NULL DEFAULT 0, pipeline_version_llm     INTEGER NOT NULL DEFAULT 0, pipeline_version_content INTEGER NOT NULL DEFAULT 0, languages        TEXT    NOT NULL DEFAULT '', languages_locked INTEGER NOT NULL DEFAULT 0, source_mtime INTEGER) STRICT;

-- Acquisition sources are separate from versions and document links. A
-- document may be observed through several places without duplicating its CAS
-- blob or changing its relationship semantics.
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

CREATE TABLE jd_areas (
    code_start   INTEGER PRIMARY KEY,
    code_end     INTEGER NOT NULL,
    name         TEXT NOT NULL,
    description  TEXT,
    position     INTEGER NOT NULL
) STRICT;

CREATE TABLE jd_categories (
    id           INTEGER PRIMARY KEY,
    area_start   INTEGER NOT NULL REFERENCES jd_areas(code_start),
    code         INTEGER NOT NULL UNIQUE,
    name         TEXT NOT NULL,
    description  TEXT,
    -- system=1 is the inbox category (and any other future protected row).
    -- The UI/API refuse to delete rows with system=1 so users cannot
    -- prune the inbox and brick ingest.
    system       INTEGER NOT NULL DEFAULT 0,
    CHECK (code BETWEEN area_start AND area_start + 9)
) STRICT;

CREATE TABLE jobs (
    id           INTEGER PRIMARY KEY,
    kind         TEXT NOT NULL,
    doc_id       INTEGER,
    payload      TEXT NOT NULL DEFAULT '{}',
    state        TEXT NOT NULL DEFAULT 'pending' CHECK (state IN ('pending','running','done','dead')),
    attempts     INTEGER NOT NULL DEFAULT 0,
    next_run_at  INTEGER NOT NULL,
    last_error   TEXT,
    created_at   INTEGER NOT NULL,
    updated_at   INTEGER NOT NULL
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
) STRICT;

CREATE TABLE saved_views (
  id           INTEGER PRIMARY KEY,
  owner_id     INTEGER NOT NULL REFERENCES users(id) ON DELETE CASCADE,
  name         TEXT NOT NULL,
	-- filter_json is a validated flat document-list query object. The
	-- API accepts only its documented keys and scalar values.
  filter_json  TEXT NOT NULL DEFAULT '{}',
  -- display: 'table' | 'card' | 'timeline'. Free-text so a v2 client
  -- can add its own without a schema bump; suchi's own UI accepts a
  -- known set.
  display      TEXT NOT NULL DEFAULT 'table',
  position     INTEGER NOT NULL DEFAULT 0,
  created_at   INTEGER NOT NULL,
  updated_at   INTEGER NOT NULL, shared INTEGER NOT NULL DEFAULT 0,
  UNIQUE(owner_id, name)
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

CREATE TABLE share_links (
  id            INTEGER PRIMARY KEY,
  token         TEXT NOT NULL UNIQUE,        -- 32-byte hex — the shared URL secret
  -- JSON array of document ids. TEXT because SQLite doesn't do JSON
  -- types natively; a JSON1 function reads it on the public-view path.
  doc_ids_json  TEXT NOT NULL,
  created_by    INTEGER NOT NULL REFERENCES users(id) ON DELETE CASCADE,
  -- Unix epoch. NULL = never expires; the handler layer enforces.
  expires_at    INTEGER,
  -- argon2id hash. NULL = no password required.
  password_hash TEXT,
  -- Label the creator saw when authoring. Human-readable, no logic
  -- attached — useful for a "your active shares" list.
  label         TEXT NOT NULL DEFAULT '',
  -- View count so operators can spot abused links. Bumped on public
  -- GET; not on preflight metadata lookups.
  view_count    INTEGER NOT NULL DEFAULT 0,
  created_at    INTEGER NOT NULL,
  -- revoked_at NULL until the creator revokes; explicit revoke makes
  -- the link 404 immediately without waiting for expiry.
  revoked_at    INTEGER
) STRICT;

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
, parent_id INTEGER REFERENCES tags(id) ON DELETE CASCADE) STRICT;

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
    capabilities TEXT NOT NULL DEFAULT '[]'
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

CREATE TABLE automations (
  id          INTEGER PRIMARY KEY,
  name        TEXT NOT NULL UNIQUE,
  order_index INTEGER NOT NULL DEFAULT 0,
  enabled     INTEGER NOT NULL DEFAULT 1,
  created_at  INTEGER NOT NULL,
  updated_at  INTEGER NOT NULL,
  preset_slug TEXT
) STRICT;


-- ---- virtual tables (fts5) ----

CREATE VIRTUAL TABLE documents_fts USING fts5 (
    title,
    content,
    content='documents',
    content_rowid='id',
    tokenize='porter unicode61 remove_diacritics 2'
);


-- ---- indexes ----

CREATE INDEX api_tokens_user ON api_tokens(user_id);

CREATE INDEX audit_actor ON audit_events(actor_kind, actor_id);

CREATE INDEX audit_object ON audit_events(object_kind, object_id);

CREATE INDEX audit_ts ON audit_events(ts);

CREATE INDEX dcfv_field ON document_custom_field_values(field_id);

CREATE INDEX decryption_passwords_owner
    ON decryption_passwords(owner_id, last_used_at DESC);

CREATE INDEX idx_email_accounts_enabled ON email_accounts(enabled);

CREATE INDEX document_correspondents_corr ON document_correspondents(correspondent_id);

CREATE INDEX document_correspondents_doc  ON document_correspondents(document_id);

CREATE INDEX document_tags_tag ON document_tags(tag_id);

CREATE UNIQUE INDEX documents_asn_uniq
    ON documents(archive_serial_number) WHERE archive_serial_number IS NOT NULL;

CREATE INDEX documents_correspondent ON documents(correspondent_id);

CREATE INDEX documents_document_type ON documents(document_type_id);

CREATE INDEX documents_email_message_id
    ON documents(email_message_id)
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
    ON documents(legacy_id) WHERE legacy_id IS NOT NULL;

CREATE INDEX documents_owner ON documents(owner_id);

CREATE UNIQUE INDEX documents_owner_original_blob
    ON documents(owner_id, original_blob)
    WHERE trashed_at IS NULL;

CREATE INDEX documents_previous_version
    ON documents(previous_version_id)
    WHERE previous_version_id IS NOT NULL;

CREATE UNIQUE INDEX documents_split_parent_part
    ON documents(split_parent_id, split_index)
    WHERE split_parent_id IS NOT NULL;

CREATE INDEX documents_storage_path  ON documents(storage_path_id);

CREATE INDEX group_members_user ON group_members(user_id);

CREATE INDEX idx_approval_defs_active        ON approval_defs(active, slug);

CREATE INDEX idx_approval_runs_active        ON approval_runs(state, deadline_at);

CREATE INDEX idx_approval_runs_doc           ON approval_runs(doc_id);

CREATE UNIQUE INDEX idx_approval_tasks_open_state
    ON approval_tasks(run_id, state_key)
    WHERE status IN ('open', 'claimed');

CREATE INDEX idx_approval_tasks_open         ON approval_tasks(status, assignee);

CREATE INDEX idx_approval_tasks_run          ON approval_tasks(run_id);

CREATE INDEX idx_approval_transitions_run    ON approval_transitions(run_id, occurred_at);

CREATE INDEX idx_saved_views_owner ON saved_views(owner_id, position);

CREATE INDEX idx_share_links_creator ON share_links(created_by, created_at DESC);

CREATE INDEX idx_share_links_expires ON share_links(expires_at) WHERE expires_at IS NOT NULL;

CREATE INDEX idx_automation_actions_aid  ON automation_actions(automation_id, order_index);

CREATE INDEX idx_automation_triggers_type ON automation_triggers(type);

CREATE INDEX idx_automation_triggers_aid  ON automation_triggers(automation_id);

CREATE INDEX idx_automations_enabled ON automations(enabled, order_index)
  WHERE enabled = 1;

CREATE INDEX idx_automations_preset_slug
  ON automations(preset_slug) WHERE preset_slug IS NOT NULL;

CREATE INDEX jobs_doc ON jobs(doc_id);

CREATE INDEX jobs_ready ON jobs(next_run_at) WHERE state = 'pending';

CREATE INDEX jobs_state ON jobs(state);

CREATE INDEX notes_document ON notes(document_id);

CREATE INDEX object_acls_lookup
  ON object_acls(object_kind, object_id, principal_kind, principal_id);

CREATE INDEX object_acls_principal
  ON object_acls(principal_kind, principal_id, object_kind);

CREATE UNIQUE INDEX document_sources_mailbox_uniq
  ON document_sources(document_id, email_account_id, detail)
  WHERE email_account_id IS NOT NULL;

CREATE INDEX document_sources_document
  ON document_sources(document_id);

CREATE INDEX document_sources_email_account
  ON document_sources(email_account_id)
  WHERE email_account_id IS NOT NULL;

CREATE INDEX render_moves_doc     ON render_moves(document_id, applied_at DESC);

CREATE INDEX render_moves_pending ON render_moves(state, created_at) WHERE state = 'pending';

CREATE INDEX sessions_expiry ON sessions(expires_at);

CREATE INDEX sessions_user ON sessions(user_id);

CREATE INDEX tags_parent ON tags(parent_id) WHERE parent_id IS NOT NULL;

-- ---- triggers ----

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
