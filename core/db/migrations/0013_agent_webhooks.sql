-- 0013_agent_webhooks.sql
--
-- Push-mode variant of the agent surface. Where the poll variant has
-- agents hitting GET /api/tasks/ on a timer, webhooks flip the flow:
-- suchi POSTs a small envelope to each subscribed URL when a matching
-- agent:* job lands. Delivery goes through the durable outbox so the
-- guarantees are the same as the rest of the pipeline (backoff,
-- attempts cap, /api/tasks/ visibility).
--
-- Secrets are AEAD-sealed with the .decrypt-key so a DB-only dump
-- doesn't leak the HMAC key. See core/crypto/aead.go.

CREATE TABLE agent_webhooks (
    id                 INTEGER PRIMARY KEY,
    owner_id           INTEGER NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    url                TEXT NOT NULL,
    kind_prefix        TEXT NOT NULL,     -- e.g. "agent:classify" or "agent:" for everything
    secret_ciphertext  BLOB NOT NULL,     -- AEAD-sealed HMAC secret
    label              TEXT,               -- optional operator-visible name
    active             INTEGER NOT NULL DEFAULT 1 CHECK (active IN (0,1)),
    created_at         INTEGER NOT NULL,
    last_delivery_at   INTEGER,
    last_status        INTEGER,            -- last HTTP status code from the receiver
    last_error         TEXT                -- tail of the last error, if any
) STRICT;

CREATE INDEX agent_webhooks_owner_active
    ON agent_webhooks(owner_id, active) WHERE active = 1;
