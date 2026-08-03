-- 0008_versioning: document versioning via previous_version_id.
--
-- Paperless-ngx #1218 sits at the top of the all-time FR list (360
-- upvotes) asking for "update document version". Real workflows:
-- lease renewal (v1 = 2024 lease, v2 = 2026 lease over same doc);
-- corrected invoice; medical report retraction; contract amendment.
--
-- Model choice (from the design pass): **linked-chain**. Each version
-- is a full documents row with a nullable previous_version_id
-- pointing at its predecessor. The most-recent row is the "head"; a
-- chain lookup walks previous_version_id back to the root.
--
-- Rationale over alternatives:
--
--   revisions-per-doc-row (JSON blob or side table)
--     Would hide old versions from every query that touches
--     documents. Breaks the "one row per stored blob" invariant and
--     makes trash/undelete/gc semantics fragile.
--
--   full-clone with tombstone
--     Bloats storage: same metadata copied for each version. Our
--     chain approach lets each version legitimately have its own
--     title, correspondent, tags — a v2 with a corrected title
--     doesn't have to fight the v1's title.
--
-- Blob storage stays in the CAS as before: two versions with the
-- same bytes still share ONE physical blob (owner-scoped dedup at
-- the row level; content-addressed dedup at the blob level).
--
-- Trash / gc semantics stay honest:
--   - Trashing a version affects only that row's trashed_at. Chain
--     integrity is unchanged.
--   - suchi gc reclaims a blob only when NO documents row references
--     it. Every version's blob is protected while any version in
--     the chain is alive.
--
-- Search: the default list handler will filter out non-head versions
-- in a follow-up commit (`WHERE NOT EXISTS (newer version)`).

ALTER TABLE documents ADD COLUMN previous_version_id INTEGER
    REFERENCES documents(id) ON DELETE SET NULL;

CREATE INDEX documents_previous_version
    ON documents(previous_version_id)
    WHERE previous_version_id IS NOT NULL;
