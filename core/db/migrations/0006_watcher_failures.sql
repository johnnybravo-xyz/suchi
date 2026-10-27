-- SPDX-License-Identifier: AGPL-3.0-or-later

-- Deterministic sidecar/schema errors remain blocked until their input changes,
-- an administrator updates the filing system's custom-field definitions, or the
-- hourly retry comes due. Transient failures keep the five-minute retry.
CREATE TABLE fs_watch_observations_new (
    root_path         TEXT NOT NULL,
    owner_id          INTEGER NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    system_id         INTEGER NOT NULL REFERENCES jd_systems(id) ON DELETE CASCADE,
    relative_path     TEXT NOT NULL,
    file_size         INTEGER NOT NULL CHECK (file_size >= 0),
    file_mtime_ns     INTEGER NOT NULL,
    sidecar_size      INTEGER NOT NULL DEFAULT -1 CHECK (sidecar_size >= -1),
    sidecar_mtime_ns  INTEGER NOT NULL DEFAULT 0,
    state             TEXT NOT NULL CHECK (state IN ('ingested', 'ignored', 'failed', 'blocked')),
    document_id       INTEGER REFERENCES documents(id) ON DELETE SET NULL,
    last_error        TEXT NOT NULL DEFAULT '',
    updated_at        INTEGER NOT NULL,
    PRIMARY KEY (root_path, owner_id, system_id, relative_path)
) STRICT;
INSERT INTO fs_watch_observations_new
SELECT * FROM fs_watch_observations;
DROP TABLE fs_watch_observations;
ALTER TABLE fs_watch_observations_new RENAME TO fs_watch_observations;

-- Rendered-path claims compare a filesystem-equivalent key (Unicode case fold
-- plus NFC) computed by core/render/view/pathclaim. Earlier journal rows keep
-- NULL keys until pathclaim fills them inside the writer.
ALTER TABLE render_moves ADD COLUMN prev_path_key TEXT;
ALTER TABLE render_moves ADD COLUMN new_path_key TEXT;
CREATE INDEX render_moves_new_path_key ON render_moves(new_path_key);
CREATE INDEX render_moves_prev_path_key ON render_moves(prev_path_key);
