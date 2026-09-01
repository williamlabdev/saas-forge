DROP POLICY IF EXISTS entry_media_published_tenant_isolation ON entry_media_published;
DROP INDEX IF EXISTS idx_entry_media_published_asset;
DROP TABLE IF EXISTS entry_media_published;

ALTER TABLE entries DROP CONSTRAINT IF EXISTS entries_published_snapshot_check;

-- Dropping the snapshot column reverts to 000016 semantics, where a published
-- entry is served straight from `payload`. Any unpublished edits sitting in
-- `payload` go live at that moment — that is inherent to the old model, not an
-- artifact of this rollback.
ALTER TABLE entries
    DROP COLUMN IF EXISTS published_version,
    DROP COLUMN IF EXISTS published_payload;
