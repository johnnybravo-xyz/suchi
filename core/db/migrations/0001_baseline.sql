-- 0001_baseline: users, JD taxonomy, documents skeleton, sessions,
-- api_tokens, plugin_kv, settings, jobs (outbox), audit_events.
--
-- This is the Phase-0 slice: enough schema to boot, authenticate, write
-- audit rows, and run the job dispatcher. It is NOT the full document
-- schema — Phase 1 adds tags/correspondents/document_types/storage_paths/
-- custom_fields/notes and the FTS5 external-content projection. Anything
-- Phase 0 code touches must exist here; anything it does not must not.
--
-- Table order: JD tables first (documents FKs into them), then users
-- (documents FKs into users too), then documents, then everything else.

-- Johnny.Decimal taxonomy. See design doc §Taxonomy.
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

-- Users. Multi-user in the data model from day one; single-user is a
-- degenerate case (one row, role=admin) chosen by first-boot flow.
CREATE TABLE users (
    id           INTEGER PRIMARY KEY,
    email        TEXT NOT NULL UNIQUE,
    display_name TEXT NOT NULL,
    role         TEXT NOT NULL CHECK (role IN ('admin','member')),
    disabled     INTEGER NOT NULL DEFAULT 0,
    -- Local-auth stores an argon2id hash here. OIDC-only users have this NULL.
    password_hash TEXT,
    created_at   INTEGER NOT NULL,
    updated_at   INTEGER NOT NULL
) STRICT;

-- Sessions (browser). Random opaque token; the signed cookie carries the
-- token, not the user id, so a leaked cookie is revocable by DELETEing
-- this row.
CREATE TABLE sessions (
    id           TEXT PRIMARY KEY,
    user_id      INTEGER NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    created_at   INTEGER NOT NULL,
    expires_at   INTEGER NOT NULL,
    last_seen_at INTEGER NOT NULL,
    user_agent   TEXT,
    ip           TEXT
) STRICT;
CREATE INDEX sessions_user ON sessions(user_id);
CREATE INDEX sessions_expiry ON sessions(expires_at);

-- API tokens (headless clients: mobile apps, agents, ingest producers).
-- token_hash is sha256(token) — the token itself is shown once at creation
-- and never stored. scopes is a comma-separated list; the small closed
-- vocabulary makes a full JSON encoder overkill here.
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
CREATE INDEX api_tokens_user ON api_tokens(user_id);

-- Documents skeleton. Phase 0 does not ingest; this table exists so
-- foreign keys in jobs and audit_events resolve, and so the schema shape
-- is committed to before Phase 1 layers metadata on top.
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
    -- points to it and is seeded by the starter tree.
    jd_category_id INTEGER NOT NULL REFERENCES jd_categories(id),
    created_at     INTEGER NOT NULL,
    updated_at     INTEGER NOT NULL,
    trashed_at     INTEGER
) STRICT;
CREATE INDEX documents_owner ON documents(owner_id);
CREATE INDEX documents_jd ON documents(jd_category_id);
CREATE UNIQUE INDEX documents_original_blob ON documents(original_blob) WHERE trashed_at IS NULL;

-- Durable outbox. Every unit of async work is a row. In-memory channels
-- are a latency optimization on top of this table; the table is the truth.
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
-- Partial index so the dispatcher poll query stays cheap even when the
-- table has millions of completed rows sitting around for audit.
CREATE INDEX jobs_ready ON jobs(next_run_at) WHERE state = 'pending';
CREATE INDEX jobs_state ON jobs(state);
CREATE INDEX jobs_doc ON jobs(doc_id);

-- Audit log. Append-only from the API middleware. before_json and
-- after_json cover metadata fields only — never document content.
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
CREATE INDEX audit_ts ON audit_events(ts);
CREATE INDEX audit_object ON audit_events(object_kind, object_id);
CREATE INDEX audit_actor ON audit_events(actor_kind, actor_id);

-- Plugin key-value scratch space. Plugins never own core tables; when
-- they need to persist anything (message-IDs for email dedup, watermark
-- cursors, etc.) they get this bucket keyed by plugin name.
CREATE TABLE plugin_kv (
    plugin_name TEXT NOT NULL,
    key         TEXT NOT NULL,
    value_json  TEXT NOT NULL,
    updated_at  INTEGER NOT NULL,
    PRIMARY KEY (plugin_name, key)
) STRICT;

-- Global instance settings, including jd_inbox_category_id (seeded by
-- the starter tree). Single-row-per-key is fine at this scale.
CREATE TABLE settings (
    key        TEXT PRIMARY KEY,
    value_json TEXT NOT NULL,
    updated_at INTEGER NOT NULL
) STRICT;
