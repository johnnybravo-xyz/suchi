-- Reserve users.avatar_sha for the next iteration — the profile-edit
-- endpoint lands display_name first (this migration lets whoami and
-- the future avatar endpoint agree on the column name without a
-- second migration touching a live table).
--
-- Type is TEXT (holds the CAS SHA-256 hex) with a NULL default so
-- every existing user row stays valid. No index — reads go through
-- users.id, not the avatar sha.

ALTER TABLE users ADD COLUMN avatar_sha TEXT;
