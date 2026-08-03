-- 0004_per_user_dedup: scope the "no duplicate original bytes" invariant
-- to a single owner instead of the whole instance.
--
-- Motivation: an-existing-dms ships a global dedup keyspace (the top-ten
-- FR cluster on their tracker calls this out — same-file-two-users
-- gets 409'd out). suchi's Phase-2 upload API followed the same
-- pattern. That's fine for a one-person deployment; wrong for a
-- household or a shared team where two people legitimately land the
-- same insurance PDF for different tax filings.
--
-- Change: drop the global-unique partial index and replace it with a
-- (owner_id, original_blob) partial-unique index. `WHERE trashed_at
-- IS NULL` preserves the "trashed doc can be undeleted on re-upload"
-- flow that's already in the upload handler.
--
-- The upload handler's alive/trashed collision queries change in
-- lockstep — this migration only fixes the schema; core/api/documents.go
-- follows in the same commit.

DROP INDEX IF EXISTS documents_original_blob;

CREATE UNIQUE INDEX documents_owner_original_blob
    ON documents(owner_id, original_blob)
    WHERE trashed_at IS NULL;
