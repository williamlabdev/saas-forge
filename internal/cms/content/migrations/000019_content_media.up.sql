-- Media assets (STRATEGY §三 Headless CMS gap 3; ADR-005).
--
-- Bytes live in S3-compatible object storage (AWS S3 / Cloudflare R2 /
-- Backblaze B2 / MinIO in dev) — the provider is deployment configuration, not
-- a code decision, because all four speak the same API. Postgres holds only
-- metadata and the storage key.
--
-- The bucket is PRIVATE. Reads go through a short-lived signed URL issued only
-- after the platform has confirmed the asset is publicly readable, so
-- "published-only" holds for the bytes and not merely for the metadata.

CREATE TABLE IF NOT EXISTS media_assets (
    id           UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id    TEXT NOT NULL,
    storage_key  TEXT NOT NULL,
    content_type TEXT NOT NULL DEFAULT '',
    size_bytes   BIGINT NOT NULL DEFAULT 0,
    -- uploaded_at stays NULL until the client confirms the direct-to-storage
    -- PUT succeeded. A row without it is a reservation, not an asset: it must
    -- never be referenced or served.
    uploaded_at  TIMESTAMPTZ,
    created_at   TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    UNIQUE (tenant_id, storage_key)
);

-- entry_media answers the question the delivery path has to ask on every read:
-- "is this asset referenced by anything published?". Deriving it by scanning
-- entries.payload would need the field key up front and could not use an index;
-- maintaining the link on write makes it a keyed lookup.
CREATE TABLE IF NOT EXISTS entry_media (
    entry_id  UUID NOT NULL REFERENCES entries(id) ON DELETE CASCADE,
    asset_id  UUID NOT NULL REFERENCES media_assets(id) ON DELETE CASCADE,
    tenant_id TEXT NOT NULL,
    PRIMARY KEY (entry_id, asset_id)
);

-- The delivery lookup direction: given an asset, find its referencing entries.
CREATE INDEX IF NOT EXISTS idx_entry_media_asset ON entry_media (tenant_id, asset_id);

-- Same tenant isolation as the rest of the content plane (see 000014).
ALTER TABLE media_assets ENABLE ROW LEVEL SECURITY;
ALTER TABLE media_assets FORCE ROW LEVEL SECURITY;
DROP POLICY IF EXISTS media_assets_tenant_isolation ON media_assets;
CREATE POLICY media_assets_tenant_isolation ON media_assets
    USING (tenant_id = app_current_tenant())
    WITH CHECK (tenant_id = app_current_tenant());

ALTER TABLE entry_media ENABLE ROW LEVEL SECURITY;
ALTER TABLE entry_media FORCE ROW LEVEL SECURITY;
DROP POLICY IF EXISTS entry_media_tenant_isolation ON entry_media;
CREATE POLICY entry_media_tenant_isolation ON entry_media
    USING (tenant_id = app_current_tenant())
    WITH CHECK (tenant_id = app_current_tenant());
