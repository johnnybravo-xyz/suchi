-- SPDX-License-Identifier: AGPL-3.0-or-later

CREATE TABLE local_auth_totp (
    user_id           INTEGER PRIMARY KEY REFERENCES users(id) ON DELETE CASCADE,
    secret_ciphertext BLOB NOT NULL,
    last_used_step    INTEGER NOT NULL CHECK (last_used_step >= 0),
    failed_attempts   INTEGER NOT NULL DEFAULT 0 CHECK (failed_attempts >= 0),
    blocked_until     INTEGER NOT NULL DEFAULT 0,
    enabled_at        INTEGER NOT NULL
) STRICT;

CREATE TABLE local_auth_recovery_codes (
    user_id     INTEGER NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    code_digest BLOB NOT NULL,
    created_at  INTEGER NOT NULL,
    PRIMARY KEY (user_id, code_digest)
) STRICT;

-- Email attachment children inherit the message's sources, whose detail names
-- the mailbox folder. filename keeps the attachment's own name; other kinds
-- carry the acquired file name in detail and leave this empty.
ALTER TABLE document_sources ADD COLUMN filename TEXT NOT NULL DEFAULT '';
