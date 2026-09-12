-- Existing mobile previews have already applied 0003. Preserve their tokens.
ALTER TABLE api_tokens
    ADD COLUMN source TEXT NOT NULL DEFAULT ''
    CHECK (source IN ('', 'mobile_pairing'));
