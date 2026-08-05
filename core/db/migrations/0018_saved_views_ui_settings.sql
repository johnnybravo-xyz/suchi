-- Saved views: named query + display config stored per user. Mobile
-- clients (and the eventual web SPA) offer "save this view" as a
-- one-tap operation over the current filter set.
CREATE TABLE saved_views (
  id           INTEGER PRIMARY KEY,
  owner_id     INTEGER NOT NULL REFERENCES users(id) ON DELETE CASCADE,
  name         TEXT NOT NULL,
  -- filter_json is the opaque blob a client's UI restores from — a
  -- {q, tags, correspondents, document_types, ordering, page_size}
  -- payload. Suchi doesn't parse it; the client owns the shape.
  filter_json  TEXT NOT NULL DEFAULT '{}',
  -- display: 'table' | 'card' | 'timeline'. Free-text so a v2 client
  -- can add its own without a schema bump; suchi's own UI accepts a
  -- known set.
  display      TEXT NOT NULL DEFAULT 'table',
  position     INTEGER NOT NULL DEFAULT 0,
  created_at   INTEGER NOT NULL,
  updated_at   INTEGER NOT NULL,
  UNIQUE(owner_id, name)
) STRICT;

CREATE INDEX idx_saved_views_owner ON saved_views(owner_id, position);

-- UI settings: opaque per-user JSON blob. A single row per user; the
-- client stores whatever shape it wants (theme choice, column order,
-- sidebar collapse state). Suchi doesn't inspect it.
CREATE TABLE ui_settings (
  owner_id     INTEGER PRIMARY KEY REFERENCES users(id) ON DELETE CASCADE,
  settings     TEXT NOT NULL DEFAULT '{}',
  updated_at   INTEGER NOT NULL
) STRICT;
