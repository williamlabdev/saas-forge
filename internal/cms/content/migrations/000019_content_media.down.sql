DROP POLICY IF EXISTS entry_media_tenant_isolation ON entry_media;
DROP POLICY IF EXISTS media_assets_tenant_isolation ON media_assets;
DROP INDEX IF EXISTS idx_entry_media_asset;
DROP TABLE IF EXISTS entry_media;
DROP TABLE IF EXISTS media_assets;
