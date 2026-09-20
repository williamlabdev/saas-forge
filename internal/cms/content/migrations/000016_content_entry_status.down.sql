DROP INDEX IF EXISTS idx_entries_tenant_type_status_created;

ALTER TABLE entries DROP CONSTRAINT IF EXISTS entries_published_at_check;
ALTER TABLE entries DROP CONSTRAINT IF EXISTS entries_status_check;

ALTER TABLE entries
    DROP COLUMN IF EXISTS published_at,
    DROP COLUMN IF EXISTS status;
