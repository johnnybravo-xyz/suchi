-- 0007_nested_tags: tags gain an optional parent_id.
--
-- Decision from the ecosystem-survey design pass:
--
--   an existing DMS #380 is a top-5 all-time FR (222 upvotes) asking
--   for nested tags. suchi's JD taxonomy already gives DOCUMENTS a
--   hierarchy — the bundle ask is orthogonal: users want to
--   organize the TAG list (which is a cross-cutting labeling axis)
--   into a shallow tree for browsing, without changing what a tag
--   MEANS on a document.
--
-- Ship a minimal shape: `tags.parent_id` as a self-referential
-- nullable FK. Adds no semantics to search (a search for the parent
-- tag returns docs tagged with that specific parent; children are
-- separate tags). Inheritance-in-search is a follow-up if it turns
-- out to be genuinely wanted; the visual-organization use case
-- doesn't need it.
--
-- ON DELETE CASCADE: deleting a parent tag also drops its children.
-- Preserves the "no dangling parent_id" invariant without a
-- separate cleanup job. Operators who don't want that reparent the
-- children first (via API PATCH).

ALTER TABLE tags ADD COLUMN parent_id INTEGER REFERENCES tags(id) ON DELETE CASCADE;

CREATE INDEX tags_parent ON tags(parent_id) WHERE parent_id IS NOT NULL;
