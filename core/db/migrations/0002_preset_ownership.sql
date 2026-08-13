-- 0002_preset_ownership.sql
--
-- Preset-owned singletons on `rules` and `automations`. A row with
-- preset_slug set is a suchi-managed immutable row, seeded by
-- ApplyPreset (or by an admin taxonomy import). User-owned rows have
-- preset_slug NULL — the classic pre-0002 shape.
--
-- Copy-on-write is enforced at the store/API layer, not by a schema
-- constraint: PATCH/DELETE on a preset-owned row forks a fresh
-- user-owned row and soft-disables the preset original. See
-- feat(jd): symbol resolution + copy-on-write seed engine.

ALTER TABLE rules       ADD COLUMN preset_slug TEXT;
ALTER TABLE automations ADD COLUMN preset_slug TEXT;

CREATE INDEX idx_rules_preset_slug        ON rules(preset_slug)        WHERE preset_slug IS NOT NULL;
CREATE INDEX idx_automations_preset_slug  ON automations(preset_slug)  WHERE preset_slug IS NOT NULL;
