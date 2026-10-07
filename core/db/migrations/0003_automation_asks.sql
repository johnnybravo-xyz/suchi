-- SPDX-License-Identifier: AGPL-3.0-or-later

CREATE TABLE automation_asks (
    automation_id INTEGER PRIMARY KEY REFERENCES automations(id) ON DELETE CASCADE,
    question      TEXT NOT NULL CHECK (length(trim(question)) BETWEEN 1 AND 1000),
    answer_type   TEXT NOT NULL CHECK (answer_type IN ('yes_no', 'choice')),
    choices       TEXT NOT NULL DEFAULT '[]'
        CHECK (json_valid(choices) AND json_type(choices) = 'array')
) STRICT;

ALTER TABLE automation_actions ADD COLUMN when_answer TEXT
    CHECK (when_answer IS NULL OR length(trim(when_answer)) BETWEEN 1 AND 40);
