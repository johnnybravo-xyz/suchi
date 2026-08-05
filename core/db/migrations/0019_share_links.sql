-- Share links: expiring, optionally password-protected pointers to
-- one or more documents. Two shapes wire through the same table:
--
--   * single-doc share — the classic path (household forwards an
--     insurance PDF to their agent); doc_ids = a single id
--   * bundle share — one link scopes multiple docs; v1 mobile
--     clients treat this as one link with N attachments
--
-- Rate-limit + audit hooks live at the handler layer, not here.
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

CREATE INDEX idx_share_links_creator ON share_links(created_by, created_at DESC);
CREATE INDEX idx_share_links_expires ON share_links(expires_at) WHERE expires_at IS NOT NULL;
