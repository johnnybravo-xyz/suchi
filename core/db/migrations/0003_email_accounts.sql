-- 0003_email_accounts.sql
--
-- First-class mail accounts. Until now the emailwatch poller was
-- boot-configured from a handful of `ingest.imap_*` settings keys —
-- one mailbox per instance, plaintext credentials in the settings JSON.
-- Full IMAP support (v0.1 blocker) needs N accounts per instance,
-- per-account owner + folder policy, and AEAD-sealed secrets so
-- password / MSAL token caches never touch disk in the clear.
--
-- sealed_secret is opaque BLOB — the shape (password bytes vs MSAL
-- cache JSON) is discriminated by auth_method at the Go layer. See
-- core/emailaccounts/secret.go for the split.
--
-- Legacy `ingest.imap_*` rows are migrated by
-- core/emailaccounts.MigrateFromLegacySettings and then deleted.

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
    attachments_only  INTEGER NOT NULL DEFAULT 0,
    from_allowlist    TEXT,
    enabled           INTEGER NOT NULL DEFAULT 1,
    last_sync_at      INTEGER,
    last_error        TEXT,
    created_at        INTEGER NOT NULL,
    updated_at        INTEGER NOT NULL
);
CREATE INDEX idx_email_accounts_enabled ON email_accounts(enabled);
