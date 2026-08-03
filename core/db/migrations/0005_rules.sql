-- 0005_rules: deterministic classification rules — the "cover 60%
-- without any LLM" engine.
--
-- Shape: one rule per row. `if_kind:if_value` names the trigger,
-- `then_kind:then_value` the action. Priority is lower-runs-first;
-- multiple matching rules all apply. Setters (set_correspondent /
-- set_document_type / set_jd_category) — later priority wins. Adders
-- (add_tag) accumulate.
--
-- Trigger kinds (Phase 2 shipping list):
--   tag              — doc already carries a tag with this name
--   correspondent    — doc's correspondent has this name
--   document_type    — doc's document_type has this name
--   title_contains   — case-insensitive substring in documents.title
--   content_contains — case-insensitive substring in documents.content
--
-- Action kinds:
--   add_tag           — upsert tag by name; add junction row
--   set_correspondent — upsert correspondent by name; set FK
--   set_document_type — upsert document_type by name; set FK
--   set_jd_category   — value is the JD code (integer as text)
--
-- All values are TEXT; the engine parses set_jd_category as an
-- integer and looks up jd_categories.code.
--
-- The design principle: rules are DATA, not code. Operators add
-- rules through the API or SQL without touching Go. The engine
-- stays boring (~200 lines) and small.

CREATE TABLE rules (
    id          INTEGER PRIMARY KEY,
    name        TEXT NOT NULL UNIQUE,
    description TEXT,
    if_kind     TEXT NOT NULL CHECK (if_kind IN (
        'tag','correspondent','document_type','title_contains','content_contains'
    )),
    if_value    TEXT NOT NULL,
    then_kind   TEXT NOT NULL CHECK (then_kind IN (
        'add_tag','set_correspondent','set_document_type','set_jd_category'
    )),
    then_value  TEXT NOT NULL,
    priority    INTEGER NOT NULL DEFAULT 100,
    enabled     INTEGER NOT NULL DEFAULT 1,
    created_at  INTEGER NOT NULL,
    updated_at  INTEGER NOT NULL
) STRICT;

CREATE INDEX rules_enabled_priority ON rules(enabled, priority) WHERE enabled = 1;
