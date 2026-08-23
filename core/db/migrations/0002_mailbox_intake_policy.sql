ALTER TABLE email_accounts
ADD COLUMN intake_policy TEXT NOT NULL
DEFAULT '{"selection":"all","content":"email_and_files"}';

UPDATE email_accounts
SET intake_policy = json_object(
    'selection', CASE
        WHEN COALESCE(from_allowlist, '') <> '' THEN 'matching'
        WHEN attachments_only = 1 THEN 'files'
        ELSE 'all'
    END,
    'content', CASE
        WHEN attachments_only = 1 THEN 'files_only'
        ELSE 'email_and_files'
    END,
    'from', COALESCE(from_allowlist, '')
);

ALTER TABLE email_accounts DROP COLUMN attachments_only;
ALTER TABLE email_accounts DROP COLUMN from_allowlist;
