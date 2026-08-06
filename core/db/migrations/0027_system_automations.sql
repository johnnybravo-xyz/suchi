-- System automations — the "ships in the box" concept for automations
-- that suchi seeds on first boot. Toggleable via `enabled` like any
-- other automation (per-row on/off is the existing pattern), but
-- undeletable — the /api/automations/{id} DELETE handler returns 409
-- when `system = 1`. Editable — power users can retune thresholds
-- through the visual builder or JSON.
--
-- system_slug is the seed's stable identifier. Idempotent seed uses
-- INSERT ... ON CONFLICT(system_slug) DO NOTHING; a later migration
-- can add new seeds without duplicating existing ones. Only meaningful
-- when system = 1, hence the partial unique index.

ALTER TABLE workflows ADD COLUMN system      INTEGER NOT NULL DEFAULT 0;
ALTER TABLE workflows ADD COLUMN system_slug TEXT;

CREATE UNIQUE INDEX workflows_system_slug_unique
    ON workflows(system_slug) WHERE system_slug IS NOT NULL;
