-- The default live-document page should stream newest rows without a temp sort.
CREATE INDEX documents_live_created
    ON documents(created_at DESC, id DESC)
    WHERE trashed_at IS NULL;
