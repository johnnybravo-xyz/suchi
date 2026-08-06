-- Add a `shared` flag to saved_views. A shared view remains
-- owner-owned (only the owner mutates), but shows up read-only on
-- every other user's list. Household use case: "Tax 2026" built once,
-- visible to both partners.
--
-- Column default 0 so every existing row stays private. The list
-- endpoint's default owner scope stays owner_id = caller; the
-- "include=shared" query flag pulls in shared views owned by others.
--
-- No new index. Cardinality of saved_views per owner tops out at 50
-- (see the CreateSavedView cap); a scalar filter on the small set
-- costs less than the write overhead of maintaining an index.

ALTER TABLE saved_views ADD COLUMN shared INTEGER NOT NULL DEFAULT 0;
