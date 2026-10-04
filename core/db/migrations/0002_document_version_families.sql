-- SPDX-License-Identifier: AGPL-3.0-or-later

ALTER TABLE documents ADD COLUMN version_family_key TEXT;

CREATE INDEX documents_version_family
    ON documents(system_id, version_family_key, id)
    WHERE version_family_key IS NOT NULL;

CREATE INDEX dcfv_documentlink_target
    ON document_custom_field_values(value_int, field_id, document_id)
    WHERE value_int IS NOT NULL;

CREATE TRIGGER dcfv_documentlink_topology_insert
BEFORE INSERT ON document_custom_field_values
WHEN NEW.value_int IS NOT NULL
 AND (SELECT data_type FROM custom_fields WHERE id = NEW.field_id) = 'documentlink'
 AND (
    NEW.document_id = NEW.value_int
    OR EXISTS (
        SELECT 1
        FROM documents AS source
        JOIN documents AS target ON target.id = NEW.value_int
        WHERE source.id = NEW.document_id
          AND source.system_id = target.system_id
          AND source.version_family_key IS NOT NULL
          AND source.version_family_key = target.version_family_key
    )
 )
BEGIN
    SELECT RAISE(ABORT, 'invalid document link topology');
END;

CREATE TRIGGER dcfv_documentlink_topology_update
BEFORE UPDATE OF document_id, field_id, value_int ON document_custom_field_values
WHEN NEW.value_int IS NOT NULL
 AND (SELECT data_type FROM custom_fields WHERE id = NEW.field_id) = 'documentlink'
 AND (
    NEW.document_id = NEW.value_int
    OR EXISTS (
        SELECT 1
        FROM documents AS source
        JOIN documents AS target ON target.id = NEW.value_int
        WHERE source.id = NEW.document_id
          AND source.system_id = target.system_id
          AND source.version_family_key IS NOT NULL
          AND source.version_family_key = target.version_family_key
    )
 )
BEGIN
    SELECT RAISE(ABORT, 'invalid document link topology');
END;

CREATE TEMP TABLE version_family_backfill_guard (
    ok INTEGER NOT NULL CONSTRAINT version_family_tree_valid CHECK (ok = 1)
) STRICT;

WITH RECURSIVE version_tree(id, root_id) AS (
    SELECT id, id
    FROM documents
    WHERE previous_version_id IS NULL
    UNION
    SELECT child.id, version_tree.root_id
    FROM documents AS child
    JOIN version_tree ON child.previous_version_id = version_tree.id
)
INSERT INTO version_family_backfill_guard(ok)
SELECT CASE WHEN EXISTS (
    SELECT 1
    FROM documents AS candidate
    WHERE candidate.previous_version_id IS NOT NULL
      AND NOT EXISTS (
          SELECT 1
          FROM version_tree
          WHERE version_tree.id = candidate.id
      )
) THEN 0 ELSE 1 END;

WITH RECURSIVE version_tree(id, root_id) AS (
    SELECT id, id
    FROM documents
    WHERE previous_version_id IS NULL
    UNION
    SELECT child.id, version_tree.root_id
    FROM documents AS child
    JOIN version_tree ON child.previous_version_id = version_tree.id
), version_families AS (
    SELECT root_id
    FROM version_tree
    GROUP BY root_id
    HAVING count(*) > 1
)
UPDATE documents
SET version_family_key = (
    SELECT 'm:' || lower(printf('%032x', version_tree.root_id))
    FROM version_tree
    WHERE version_tree.id = documents.id
)
WHERE id IN (
    SELECT version_tree.id
    FROM version_tree
    JOIN version_families USING (root_id)
);

DROP TABLE version_family_backfill_guard;
