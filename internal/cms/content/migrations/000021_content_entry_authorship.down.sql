-- Reverses 000021. Rows survive; only the authorship record is lost, which is
-- unrecoverable by design — there is nowhere else it was written down.

ALTER TABLE entries
    DROP CONSTRAINT IF EXISTS entries_published_by_snapshot_check;

ALTER TABLE entries
    DROP COLUMN IF EXISTS published_by,
    DROP COLUMN IF EXISTS updated_by,
    DROP COLUMN IF EXISTS created_by;
