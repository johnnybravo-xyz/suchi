-- Add an independent suspension state for preset-owned automations.
-- Disabled remains the operator's choice and survives preset transitions.
ALTER TABLE automations ADD COLUMN suspended INTEGER NOT NULL DEFAULT 0
    CHECK (suspended IN (0, 1));
