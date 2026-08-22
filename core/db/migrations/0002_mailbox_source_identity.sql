-- Keep acquisition history after a mailbox is deleted while allowing its
-- current display name to follow account renames.
CREATE TABLE document_sources_v2 (
    id               INTEGER PRIMARY KEY,
    document_id      INTEGER NOT NULL REFERENCES documents(id) ON DELETE CASCADE,
    kind             TEXT NOT NULL CHECK (kind IN (
                         'upload', 'api', 'mailbox', 'watched_folder',
                         'import'
                     )),
    label            TEXT NOT NULL DEFAULT '',
    detail           TEXT NOT NULL DEFAULT '',
    observed_at      INTEGER NOT NULL,
    email_account_id INTEGER REFERENCES email_accounts(id) ON DELETE SET NULL,
    CHECK (email_account_id IS NULL OR kind = 'mailbox')
) STRICT;

INSERT INTO document_sources_v2(
    id, document_id, kind, label, detail, observed_at, email_account_id
)
SELECT ds.id, ds.document_id, ds.kind, ds.label, ds.detail, ds.observed_at,
       CASE WHEN ds.kind = 'mailbox' THEN (
           SELECT ea.id
           FROM email_accounts ea
           JOIN documents d ON d.id = ds.document_id
           WHERE ea.owner_id = d.owner_id
             AND ds.detail = CASE
                 WHEN ea.username = '' THEN ea.folder
                 ELSE ea.username || ' / ' || ea.folder
             END
           ORDER BY ea.id
           LIMIT 1
       ) END
FROM document_sources ds;

-- A label rename must not create a second acquisition record for the same
-- mailbox and folder. Keep the first observation if an old database already
-- contains both labels.
DELETE FROM document_sources_v2
WHERE email_account_id IS NOT NULL
  AND id NOT IN (
      SELECT MIN(id)
      FROM document_sources_v2
      WHERE email_account_id IS NOT NULL
      GROUP BY document_id, email_account_id, detail
  );

DROP TABLE document_sources;
ALTER TABLE document_sources_v2 RENAME TO document_sources;

CREATE UNIQUE INDEX document_sources_mailbox_uniq
    ON document_sources(document_id, email_account_id, detail)
    WHERE email_account_id IS NOT NULL;

CREATE INDEX document_sources_document
    ON document_sources(document_id);

CREATE INDEX document_sources_email_account
    ON document_sources(email_account_id)
    WHERE email_account_id IS NOT NULL;
