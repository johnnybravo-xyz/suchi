-- 0020_automations: trigger→conditions→actions engine for document
-- automations. Distinct from the state-machine approvals engine in
-- 0016_workflow.sql (renamed to approval_* by 0022): this one fires
-- on job events and applies bulk metadata operations.
--
-- Named for what they do; the sibling engine at core/approvals/ owns
-- the state-machine "approvals" surface (routing/sign-off). The tables
-- this migration defines (workflows, workflow_triggers, workflow_actions)
-- retain the "workflow" prefix because those names are the an-existing-dms
-- compat surface — mobile clients speak that vocabulary.
-- URLs: /api/automations/ (this) vs /api/approvals/ (state-machine).
--
-- Shape:
--   workflows          one row per rule; carries name, order, enabled
--   workflow_triggers  one row per (workflow, trigger_type); a workflow
--                      can fire on multiple event kinds (consumption,
--                      document_added, document_updated)
--   workflow_actions   one row per (workflow, kind); actions run in
--                      order_index sequence for the workflow

CREATE TABLE workflows (
  id          INTEGER PRIMARY KEY,
  name        TEXT NOT NULL UNIQUE,
  order_index INTEGER NOT NULL DEFAULT 0,
  enabled     INTEGER NOT NULL DEFAULT 1,
  created_at  INTEGER NOT NULL,
  updated_at  INTEGER NOT NULL
) STRICT;

CREATE INDEX idx_workflows_enabled ON workflows(enabled, order_index)
  WHERE enabled = 1;

CREATE TABLE workflow_triggers (
  id             INTEGER PRIMARY KEY,
  workflow_id    INTEGER NOT NULL REFERENCES workflows(id) ON DELETE CASCADE,
  -- consumption | document_added | document_updated
  type           TEXT NOT NULL,
  -- Optional filters. NULL = "any". Every non-null field must match.
  filter_path       TEXT,   -- glob against source path (consumption trigger)
  filter_filename   TEXT,   -- glob against filename (consumption trigger)
  filter_mailrule_id INTEGER, -- consumption via mail-intake rule id
  filter_tag_id      INTEGER, -- doc carries this tag (added/updated)
  filter_corr_id     INTEGER, -- doc has this correspondent (added/updated)
  filter_doctype_id  INTEGER, -- doc has this document_type (added/updated)
  filter_content_re  TEXT,   -- regex against documents.content
  created_at   INTEGER NOT NULL
) STRICT;

CREATE INDEX idx_workflow_triggers_wf   ON workflow_triggers(workflow_id);
CREATE INDEX idx_workflow_triggers_type ON workflow_triggers(type);

CREATE TABLE workflow_actions (
  id           INTEGER PRIMARY KEY,
  workflow_id  INTEGER NOT NULL REFERENCES workflows(id) ON DELETE CASCADE,
  order_index  INTEGER NOT NULL DEFAULT 0,
  -- assign_title | assign_tags | assign_correspondent | assign_document_type
  -- | assign_storage_path | assign_owner | assign_custom_field
  -- | remove_tags | remove_correspondents | remove_document_type
  -- | remove_storage_path | remove_owner | remove_custom_field
  kind         TEXT NOT NULL,
  -- Params live in a small JSON blob. Shape depends on kind:
  --   assign_title            {"template": "{{correspondent}} — {{title}}"}
  --   assign_tags             {"tag_ids": [1,2,3]}
  --   assign_correspondent    {"correspondent_id": 5}
  --   assign_document_type    {"document_type_id": 7}
  --   assign_storage_path     {"storage_path_id": 3}
  --   assign_owner            {"owner_id": 2}
  --   assign_custom_field     {"field_id": 4, "value": "..."}
  --   remove_tags             {"tag_ids": [1,2]}
  --   remove_correspondents   {"correspondent_ids": [5]}
  --   remove_document_type    {}
  --   remove_storage_path     {}
  --   remove_owner            {}
  --   remove_custom_field     {"field_id": 4}
  params_json  TEXT NOT NULL DEFAULT '{}',
  created_at   INTEGER NOT NULL
) STRICT;

CREATE INDEX idx_workflow_actions_wf ON workflow_actions(workflow_id, order_index);
