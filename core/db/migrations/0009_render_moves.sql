-- 0009_render_moves.sql
--
-- Persistent audit trail for rendered-view symlink moves. Every time a
-- metadata change re-renders a document to a new storage-path target,
-- we insert a pending row BEFORE touching the filesystem, do the move,
-- then mark the row applied. On boot, `render_moves WHERE state='pending'`
-- is the recovery queue: reconcile against on-disk state and either
-- finish the move or park the row as failed.
--
-- Design rationale (Immich-inspired, see notes/suchi-plan.md §render):
--   - Crash after INSERT-pending but before symlink move → boot sees a
--     pending row, checks whether prev_path or new_path exists on disk,
--     and resolves.
--   - Crash after symlink move but before UPDATE state=applied → same
--     recovery path picks the new_path branch.
--   - Same-path re-renders are skipped in the mover; they never write
--     a row so the table stays scoped to actual moves.

CREATE TABLE render_moves (
    id           INTEGER PRIMARY KEY,
    document_id  INTEGER NOT NULL REFERENCES documents(id) ON DELETE CASCADE,
    prev_path    TEXT NOT NULL,   -- relative to renderDir; "" for the first render of a doc
    new_path     TEXT NOT NULL,   -- relative to renderDir
    state        TEXT NOT NULL CHECK (state IN ('pending','applied','failed')),
    created_at   INTEGER NOT NULL,
    applied_at   INTEGER,
    err          TEXT
) STRICT;

CREATE INDEX render_moves_pending ON render_moves(state, created_at) WHERE state = 'pending';
CREATE INDEX render_moves_doc     ON render_moves(document_id, applied_at DESC);
