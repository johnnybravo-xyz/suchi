-- suchi: rebuild-tables

-- Add the final stable-v1 OIDC binding columns without changing durable user IDs.
-- A temporary copy avoids ALTER TABLE schema text so adopted databases match the
-- declarative baseline byte-for-byte in sqlite_schema.
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
    id, email, display_name, role, disabled, password_hash, created_at,
    updated_at, avatar_sha, capabilities, oidc_issuer, oidc_subject
)
SELECT id, email, display_name, role, disabled, password_hash, created_at,
       updated_at, avatar_sha, capabilities, NULL, NULL
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

-- Retained security records share the audit table but are excluded from ordinary
-- feed pruning. Existing beta records remain ordinary rows.
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
