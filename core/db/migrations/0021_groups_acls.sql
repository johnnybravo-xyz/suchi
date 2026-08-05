-- 0021_groups_acls: Phase 6 permissions foundation.
--
-- Three tables, one bitmask, no ORM:
--   groups           named collection of users
--   group_members    user ↔ group junction
--   object_acls      permission grants on individual objects
--
-- Enforcement is opt-in — an empty object_acls table for a given
-- object means "fall back to legacy behavior" (owner or admin).
-- Non-empty means "only these principals with these bits".
--
-- Design choices worth reading before you change this file:
--
-- 1. Permission bits are: view=1, change=2, delete=4. Union-of-grants
--    semantics (a user's effective perms are the OR of every grant
--    that names them or a group they belong to). Owner has all bits
--    implicitly; admin bypasses ACL checks entirely.
--
-- 2. object_kind is a small closed vocabulary — 'document', 'tag',
--    'correspondent', 'document_type', 'storage_path'. We do NOT use
--    a foreign key here because that would require one per kind, and
--    the polymorphic column is standard for auth-log-style tables.
--    Referential integrity is enforced at the application layer via
--    the CHECK constraint.
--
-- 3. principal_kind is 'user' or 'group'. Same shape rationale.
--
-- 4. Grants can be revoked; audit_events already captures who did it.
--    No soft-delete on ACLs — a revoke is a plain DELETE.

CREATE TABLE groups (
  id           INTEGER PRIMARY KEY,
  name         TEXT NOT NULL UNIQUE,
  description  TEXT NOT NULL DEFAULT '',
  created_at   INTEGER NOT NULL,
  updated_at   INTEGER NOT NULL
) STRICT;

CREATE TABLE group_members (
  group_id   INTEGER NOT NULL REFERENCES groups(id) ON DELETE CASCADE,
  user_id    INTEGER NOT NULL REFERENCES users(id)  ON DELETE CASCADE,
  created_at INTEGER NOT NULL,
  PRIMARY KEY (group_id, user_id)
) STRICT;

CREATE INDEX group_members_user ON group_members(user_id);

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

CREATE INDEX object_acls_lookup
  ON object_acls(object_kind, object_id, principal_kind, principal_id);

-- Reverse-lookup index for "list all objects this principal can touch"
-- — used by the list-query rewriter in batch 2.
CREATE INDEX object_acls_principal
  ON object_acls(principal_kind, principal_id, object_kind);
