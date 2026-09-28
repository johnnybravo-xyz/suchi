-- suchi: rebuild-tables

-- Some archives predate the removal of the agent-webhook experiment. The
-- legacy profile is admitted only when this table is empty.
DROP TABLE IF EXISTS agent_webhooks;

-- Rebuild this unchanged table to remove comments from the abandoned
-- create_agent_task action that survive in older schema-3 archives.
CREATE TABLE automation_actions_copy AS SELECT * FROM automation_actions;
DROP TABLE automation_actions;
CREATE TABLE automation_actions (
  id             INTEGER PRIMARY KEY,
  automation_id  INTEGER NOT NULL REFERENCES automations(id) ON DELETE CASCADE,
  order_index  INTEGER NOT NULL DEFAULT 0,
  -- assign_title | assign_tags | assign_correspondent | assign_document_type
  -- | assign_jd_category | assign_storage_path | assign_owner | assign_custom_field
  -- | discard
  -- | remove_tags | remove_correspondents | remove_document_type
  -- | remove_storage_path | remove_custom_field
  kind         TEXT NOT NULL,
  -- Params live in a small JSON blob. Shape depends on kind:
  --   assign_title            {"template": "{{correspondent}} — {{title}}"}
  --   assign_tags             {"tag_ids": [1,2,3]}
  --   assign_correspondent    {"correspondent_id": 5}
  --   assign_document_type    {"document_type_id": 7}
  --   assign_jd_category      {"jd_category_id": 8}
  --   assign_storage_path     {"storage_path_id": 3}
  --   assign_owner            {"owner_id": 2}
  --   assign_custom_field     {"field_id": 4, "value": "..."}
  --   discard                 {}
  --   remove_tags             {"tag_ids": [1,2]}
  --   remove_correspondents   {"correspondent_ids": [5]}
  --   remove_document_type    {}
  --   remove_storage_path     {}
  --   remove_custom_field     {"field_id": 4}
  params_json  TEXT NOT NULL DEFAULT '{}',
  created_at   INTEGER NOT NULL
) STRICT;
INSERT INTO automation_actions (id, automation_id, order_index, kind, params_json, created_at)
SELECT id, automation_id, order_index, kind, params_json, created_at FROM automation_actions_copy;
DROP TABLE automation_actions_copy;
CREATE INDEX idx_automation_actions_aid  ON automation_actions(automation_id, order_index);

-- Preserve tag descendants when a parent is deleted. Beta.3 shipped the
-- cascading self-reference, so rebuild the table before adopting stable v1.
DROP TRIGGER IF EXISTS automation_triggers_filter_tag_id_insert;
DROP TRIGGER IF EXISTS automation_triggers_filter_tag_id_update;
DROP TRIGGER IF EXISTS document_tags_reference_insert;
DROP TRIGGER IF EXISTS document_tags_reference_update;

CREATE TABLE tags_new (
    id                   INTEGER PRIMARY KEY,
    name                 TEXT NOT NULL,
    slug                 TEXT NOT NULL,
    color                TEXT NOT NULL DEFAULT '#a6cee3',
    matching_algorithm   INTEGER NOT NULL DEFAULT 0,
    match                TEXT NOT NULL DEFAULT '',
    is_insensitive       INTEGER NOT NULL DEFAULT 1,
    is_inbox_tag         INTEGER NOT NULL DEFAULT 0,
    created_at           INTEGER NOT NULL,
    updated_at           INTEGER NOT NULL,
    parent_id INTEGER REFERENCES tags(id) ON DELETE SET NULL,
    system_id INTEGER NOT NULL REFERENCES jd_systems(id),
    UNIQUE(system_id,name),
    UNIQUE(system_id,slug)
) STRICT;
INSERT INTO tags_new (id, name, slug, color, matching_algorithm, match, is_insensitive, is_inbox_tag, created_at, updated_at, parent_id, system_id)
SELECT id, name, slug, color, matching_algorithm, match, is_insensitive, is_inbox_tag, created_at, updated_at, parent_id, system_id FROM tags;
DROP TABLE tags;
ALTER TABLE tags_new RENAME TO tags;

CREATE INDEX tags_parent ON tags(parent_id) WHERE parent_id IS NOT NULL;

CREATE TRIGGER automation_triggers_filter_tag_id_insert BEFORE INSERT ON automation_triggers
WHEN NEW.filter_tag_id IS NOT NULL AND NEW.filter_tag_id<>0 AND (SELECT system_id FROM automations WHERE id=NEW.automation_id) IS NOT (SELECT system_id FROM tags WHERE id=NEW.filter_tag_id)
BEGIN SELECT RAISE(ABORT,'cross-system filter tag id'); END;

CREATE TRIGGER automation_triggers_filter_tag_id_update BEFORE UPDATE OF automation_id,filter_tag_id ON automation_triggers
WHEN NEW.filter_tag_id IS NOT NULL AND NEW.filter_tag_id<>0 AND (SELECT system_id FROM automations WHERE id=NEW.automation_id) IS NOT (SELECT system_id FROM tags WHERE id=NEW.filter_tag_id)
BEGIN SELECT RAISE(ABORT,'cross-system filter tag id'); END;

CREATE TRIGGER document_tags_reference_insert BEFORE INSERT ON document_tags
WHEN NEW.tag_id IS NOT NULL AND (SELECT system_id FROM documents WHERE id=NEW.document_id) IS NOT (SELECT system_id FROM tags WHERE id=NEW.tag_id)
BEGIN SELECT RAISE(ABORT,'cross-system reference'); END;

CREATE TRIGGER document_tags_reference_update BEFORE UPDATE OF document_id,tag_id ON document_tags
WHEN NEW.tag_id IS NOT NULL AND (SELECT system_id FROM documents WHERE id=NEW.document_id) IS NOT (SELECT system_id FROM tags WHERE id=NEW.tag_id)
BEGIN SELECT RAISE(ABORT,'cross-system reference'); END;

CREATE TRIGGER tags_parent_insert BEFORE INSERT ON tags
WHEN NEW.parent_id IS NOT NULL AND NEW.system_id IS NOT (SELECT system_id FROM tags WHERE id=NEW.parent_id)
BEGIN SELECT RAISE(ABORT,'cross-system parent'); END;

CREATE TRIGGER tags_parent_update BEFORE UPDATE OF system_id,parent_id ON tags
WHEN NEW.parent_id IS NOT NULL AND NEW.system_id IS NOT (SELECT system_id FROM tags WHERE id=NEW.parent_id)
BEGIN SELECT RAISE(ABORT,'cross-system parent'); END;

CREATE TRIGGER tags_system_immutable BEFORE UPDATE OF system_id ON tags
WHEN NEW.system_id IS NOT OLD.system_id
BEGIN SELECT RAISE(ABORT,'system ownership is immutable'); END;

CREATE TRIGGER tags_system_replace BEFORE INSERT ON tags
WHEN EXISTS (SELECT 1 FROM tags WHERE id=NEW.id AND system_id IS NOT NEW.system_id)
BEGIN SELECT RAISE(ABORT,'system ownership is immutable'); END;

-- Add an independent suspension state for preset-owned automations.
-- Disabled remains the operator's choice and survives preset transitions.
ALTER TABLE automations ADD COLUMN suspended INTEGER NOT NULL DEFAULT 0
    CHECK (suspended IN (0, 1));
