DROP INDEX IF EXISTS idx_entries_tenant_type_locale_status_created;
DROP INDEX IF EXISTS idx_entries_group_locale;

ALTER TABLE entries DROP CONSTRAINT IF EXISTS entries_locale_check;

ALTER TABLE entries
    DROP COLUMN IF EXISTS translation_group_id,
    DROP COLUMN IF EXISTS locale;
