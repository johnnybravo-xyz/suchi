-- 0004_automations_email_triggers.sql
--
-- Widen automation_triggers so mail routing lives inside the existing
-- automations engine instead of a parallel mail-rules table. Producers
-- (emailwatch importOne) put email_from / email_subject / email_folder /
-- email_has_attachment on the post-ingest job payload; the automations
-- matcher gates on the corresponding filter_ columns. Reuses one engine
-- per the "reuse engines, don't parallel one" design principle.
--
-- filter_email_has_attachment is three-state — NULL means "don't care",
-- 1 means "must have attachments", 0 means "must not". Storing as
-- INTEGER lets sqlite keep the tri-state cheaply without a CHECK.

ALTER TABLE automation_triggers ADD COLUMN filter_email_from TEXT;
ALTER TABLE automation_triggers ADD COLUMN filter_email_subject TEXT;
ALTER TABLE automation_triggers ADD COLUMN filter_email_folder TEXT;
ALTER TABLE automation_triggers ADD COLUMN filter_email_has_attachment INTEGER;
