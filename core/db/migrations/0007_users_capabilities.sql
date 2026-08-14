-- 0007_users_capabilities.sql
--
-- Per-user capabilities column. JSON array of slugs; validated at
-- write time by core/authz.ParseWire against the KnownCapabilities
-- whitelist. Admins are implicitly capable of everything, so
-- capabilities is only meaningful for role='member'.
--
-- Default '[]' keeps every existing user at their pre-migration
-- permissions (members: no extra caps; admins: still fully capable
-- via role short-circuit).

ALTER TABLE users
  ADD COLUMN capabilities TEXT NOT NULL DEFAULT '[]';
