-- suchi: rebuild-tables
-- SPDX-License-Identifier: AGPL-3.0-or-later

-- Document types were a second flat label vocabulary inherited from Paperless.
-- Preserve every type as a namespaced tag before removing that duplicate model.
INSERT INTO tags(
    name, slug, color, matching_algorithm, match, is_insensitive,
    is_inbox_tag, created_at, updated_at, parent_id, system_id
)
SELECT
    'type:' || dt.name,
    CASE
        WHEN EXISTS (
            SELECT 1 FROM tags conflict
            WHERE conflict.system_id = dt.system_id
              AND conflict.slug = 'type-' || dt.slug
        ) THEN 'type-document-type-' || dt.id
        ELSE 'type-' || dt.slug
    END,
    '#a6cee3', dt.matching_algorithm, dt.match, dt.is_insensitive,
    0, dt.created_at, dt.updated_at, NULL, dt.system_id
FROM document_types dt
WHERE NOT EXISTS (
    SELECT 1 FROM tags existing
    WHERE existing.system_id = dt.system_id
      AND existing.name = 'type:' || dt.name
);

CREATE TEMP TABLE _document_type_tags (
    document_type_id INTEGER PRIMARY KEY,
    system_id INTEGER NOT NULL,
    old_name TEXT NOT NULL,
    tag_id INTEGER NOT NULL,
    tag_name TEXT NOT NULL
) STRICT;

INSERT INTO _document_type_tags(document_type_id, system_id, old_name, tag_id, tag_name)
SELECT dt.id, dt.system_id, dt.name, t.id, t.name
FROM document_types dt
JOIN tags t
  ON t.system_id = dt.system_id
 AND t.name = 'type:' || dt.name;

-- Existing assignments become ordinary, human-owned tags.
INSERT INTO document_tags(document_id, tag_id, classifier_owned)
SELECT d.id, m.tag_id, 0
FROM documents d
JOIN _document_type_tags m ON m.document_type_id = d.document_type_id
WHERE d.document_type_id IS NOT NULL
ON CONFLICT(document_id, tag_id) DO UPDATE SET classifier_owned = 0;

-- A native trigger only has one tag predicate. A type-only predicate maps
-- exactly; an existing different tag plus a type was an AND expression and is
-- suspended rather than silently broadened.
UPDATE automations
SET suspended = 1, updated_at = unixepoch()
WHERE id IN (
    SELECT tr.automation_id
    FROM automation_triggers tr
    JOIN _document_type_tags m ON m.document_type_id = tr.filter_doctype_id
    WHERE tr.filter_tag_id IS NOT NULL
      AND tr.filter_tag_id <> 0
      AND tr.filter_tag_id <> m.tag_id
);

UPDATE automation_triggers AS tr
SET filter_tag_id = (
    SELECT m.tag_id
    FROM _document_type_tags m
    WHERE m.document_type_id = tr.filter_doctype_id
)
WHERE tr.filter_doctype_id IS NOT NULL
  AND tr.filter_doctype_id <> 0
  AND (tr.filter_tag_id IS NULL OR tr.filter_tag_id = 0);

-- Assignment/removal actions keep their effect by targeting the converted tags.
UPDATE automations
SET suspended = 1, updated_at = unixepoch()
WHERE id IN (
    SELECT a.automation_id
    FROM automation_actions a
    WHERE a.kind = 'assign_document_type'
      AND NOT EXISTS (
          SELECT 1 FROM _document_type_tags m
          WHERE m.document_type_id = CAST(json_extract(a.params_json, '$.document_type_id') AS INTEGER)
      )
);

DELETE FROM automation_actions
WHERE kind = 'assign_document_type'
  AND NOT EXISTS (
      SELECT 1 FROM _document_type_tags m
      WHERE m.document_type_id = CAST(json_extract(automation_actions.params_json, '$.document_type_id') AS INTEGER)
  );

UPDATE automation_actions AS a
SET kind = 'assign_tags',
    params_json = json_object(
        'tag_ids',
        json_array((
            SELECT m.tag_id
            FROM _document_type_tags m
            WHERE m.document_type_id = CAST(json_extract(a.params_json, '$.document_type_id') AS INTEGER)
        ))
    )
WHERE a.kind = 'assign_document_type';

DELETE FROM automation_actions
WHERE kind = 'remove_document_type'
  AND NOT EXISTS (
      SELECT 1
      FROM automations owner
      JOIN _document_type_tags m ON m.system_id = owner.system_id
      WHERE owner.id = automation_actions.automation_id
  );

UPDATE automation_actions AS a
SET kind = 'remove_tags',
    params_json = json_object(
        'tag_ids',
        json((
            SELECT json_group_array(m.tag_id)
            FROM automations owner
            JOIN _document_type_tags m ON m.system_id = owner.system_id
            WHERE owner.id = a.automation_id
        ))
    )
WHERE a.kind = 'remove_document_type';

-- Title templates have a generic tags variable after this migration.
UPDATE automation_actions
SET params_json = replace(
    replace(params_json, '{{document_type}}', '{{tags}}'),
    '{{ document_type }}', '{{tags}}'
)
WHERE kind = 'assign_title'
  AND (params_json LIKE '%{{document_type}}%'
       OR params_json LIKE '%{{ document_type }}%');

UPDATE storage_paths
SET path = replace(
    replace(path, '{{document_type}}', '{{tag_list}}'),
    '{{ document_type }}', '{{ tag_list }}'
), updated_at = unixepoch()
WHERE path LIKE '%{{document_type}}%'
   OR path LIKE '%{{ document_type }}%';

-- API-created saved views store quoted rich-query values. Fold every former
-- type clause into the corresponding namespaced tag clause.
WITH RECURSIVE
numbered AS (
    SELECT system_id, old_name, tag_name,
           row_number() OVER (PARTITION BY system_id ORDER BY document_type_id) AS position
    FROM _document_type_tags
),
rewritten(view_id, system_id, position, query) AS (
    SELECT sv.id, sv.system_id, 0, COALESCE(json_extract(sv.filter_json, '$.q'), '')
    FROM saved_views sv
    WHERE json_valid(sv.filter_json)
      AND json_type(sv.filter_json, '$.q') = 'text'
    UNION ALL
    SELECT r.view_id, r.system_id, r.position + 1,
           replace(
               r.query,
               'type:"' || replace(replace(n.old_name, '\', '\\'), '"', '\"') || '"',
               'tag:"' || replace(replace(n.tag_name, '\', '\\'), '"', '\"') || '"'
           )
    FROM rewritten r
    JOIN numbered n
      ON n.system_id = r.system_id
     AND n.position = r.position + 1
),
final_queries AS (
    SELECT r.view_id, r.query
    FROM rewritten r
    WHERE NOT EXISTS (
        SELECT 1 FROM numbered n
        WHERE n.system_id = r.system_id
          AND n.position > r.position
    )
)
UPDATE saved_views
SET filter_json = json_set(
    filter_json,
    '$.q',
    (SELECT f.query FROM final_queries f WHERE f.view_id = saved_views.id)
)
WHERE id IN (SELECT view_id FROM final_queries);

-- Paperless-style flat saved-view filters may also exist. Normalize the tag
-- list to the CSV form accepted by the API and remove the retired key.
UPDATE saved_views AS sv
SET filter_json = json_remove(
    json_set(
        sv.filter_json,
        '$.tags__id__in',
        CASE json_type(sv.filter_json, '$.tags__id__in')
            WHEN 'array' THEN
                COALESCE((
                    SELECT group_concat(CAST(value AS TEXT), ',')
                    FROM json_each(sv.filter_json, '$.tags__id__in')
                ) || ',', '') || CAST(m.tag_id AS TEXT)
            WHEN 'null' THEN CAST(m.tag_id AS TEXT)
            ELSE
                CASE
                    WHEN COALESCE(CAST(json_extract(sv.filter_json, '$.tags__id__in') AS TEXT), '') = ''
                        THEN CAST(m.tag_id AS TEXT)
                    ELSE CAST(json_extract(sv.filter_json, '$.tags__id__in') AS TEXT) || ',' || CAST(m.tag_id AS TEXT)
                END
        END
    ),
    '$.document_type__id'
)
FROM _document_type_tags m
WHERE json_valid(sv.filter_json)
  AND CAST(json_extract(sv.filter_json, '$.document_type__id') AS INTEGER) = m.document_type_id;

-- ACL grants follow the converted object. Existing tag grants combine because
-- read/write/manage permissions are nested bit sets.
CREATE TABLE object_acls_new (
  id             INTEGER PRIMARY KEY,
  object_kind    TEXT    NOT NULL CHECK (object_kind IN (
      'document', 'tag', 'correspondent', 'storage_path'
  )),
  object_id      INTEGER NOT NULL,
  principal_kind TEXT    NOT NULL CHECK (principal_kind IN ('user', 'group')),
  principal_id   INTEGER NOT NULL,
  perm_bits      INTEGER NOT NULL DEFAULT 0,
  created_at     INTEGER NOT NULL,
  created_by     INTEGER REFERENCES users(id) ON DELETE SET NULL,
  UNIQUE (object_kind, object_id, principal_kind, principal_id)
) STRICT;

INSERT INTO object_acls_new(id, object_kind, object_id, principal_kind, principal_id, perm_bits, created_at, created_by)
SELECT id, object_kind, object_id, principal_kind, principal_id, perm_bits, created_at, created_by
FROM object_acls
WHERE object_kind <> 'document_type';

INSERT INTO object_acls_new(id, object_kind, object_id, principal_kind, principal_id, perm_bits, created_at, created_by)
SELECT acl.id, 'tag', m.tag_id, acl.principal_kind, acl.principal_id,
       acl.perm_bits, acl.created_at, acl.created_by
FROM object_acls acl
JOIN _document_type_tags m ON m.document_type_id = acl.object_id
WHERE acl.object_kind = 'document_type'
ON CONFLICT(object_kind, object_id, principal_kind, principal_id) DO UPDATE SET
    perm_bits = object_acls_new.perm_bits | excluded.perm_bits,
    created_at = min(object_acls_new.created_at, excluded.created_at);

DROP TABLE object_acls;
ALTER TABLE object_acls_new RENAME TO object_acls;

-- Pending type suggestions cannot be applied after the vocabulary disappears.
-- End them explicitly; queued approval jobs already ignore terminal runs.
UPDATE approval_tasks
SET status = 'expired', resolved_by = 'system:migration', resolved_at = unixepoch()
WHERE run_id IN (
    SELECT r.id
    FROM approval_runs r
    JOIN approval_defs d ON d.id = r.def_id
    WHERE d.slug = 'document-change'
      AND json_valid(r.vars_json)
      AND r.state = 'running'
      AND json_extract(r.vars_json, '$.field') = 'document_type'
)
  AND status IN ('open', 'claimed');

UPDATE approval_runs
SET state = 'cancelled', ended_at = unixepoch(), deadline_at = NULL, revision = revision + 1
WHERE id IN (
    SELECT r.id
    FROM approval_runs r
    JOIN approval_defs d ON d.id = r.def_id
    WHERE d.slug = 'document-change'
      AND json_valid(r.vars_json)
      AND r.state = 'running'
      AND json_extract(r.vars_json, '$.field') = 'document_type'
);

-- Remove the native schema only after every live reference has moved.
DROP INDEX documents_document_type;
DROP TRIGGER automation_triggers_filter_doctype_id_insert;
DROP TRIGGER automation_triggers_filter_doctype_id_update;
DROP TRIGGER documents_document_type_id_insert;
DROP TRIGGER documents_document_type_id_update;
DROP TRIGGER documents_document_type_revision;

ALTER TABLE automation_triggers DROP COLUMN filter_doctype_id;
ALTER TABLE documents DROP COLUMN document_type_id;
ALTER TABLE documents DROP COLUMN document_type_revision;
DROP TABLE document_types;

DROP TABLE _document_type_tags;
