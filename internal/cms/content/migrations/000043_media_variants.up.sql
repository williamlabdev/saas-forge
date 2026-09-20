-- ADR-019: media transforms.
--
-- One row per (asset, preset) describing a derived rendition of an uploaded
-- image, plus one `original` row where the worker records what it measured
-- about the source. Rows are inserted in the same transaction that marks the
-- asset uploaded (service.CompleteMediaUpload), so "uploaded image" and
-- "has variant rows" are the same fact.
--
-- No `processing` state, on purpose. The worker claims a row with
-- SELECT ... FOR UPDATE SKIP LOCKED inside the transaction that also writes
-- the result, exactly as content_entry_schedules does (000041, ADR-017 §3). A
-- crash between claim and commit rolls the claim back and the row is pending
-- again; nothing needs reaping.

CREATE TABLE media_variants (
    asset_id        UUID        NOT NULL REFERENCES media_assets(id) ON DELETE CASCADE,
    tenant_id       TEXT        NOT NULL,
    preset          TEXT        NOT NULL,
    state           TEXT        NOT NULL DEFAULT 'pending',
    -- Where the bytes are once state = 'done'. For the `original` row this is
    -- the asset's own storage_key from the start.
    storage_key     TEXT,
    content_type    TEXT,
    size_bytes      BIGINT,
    width_px        INTEGER,
    height_px       INTEGER,
    attempts        INTEGER     NOT NULL DEFAULT 0,
    -- When the worker may next pick the row up. NULL = now. The pending
    -- partial index below orders by it.
    next_attempt_at TIMESTAMPTZ,
    error           TEXT,
    skip_reason     TEXT,
    created_at      TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at      TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    PRIMARY KEY (asset_id, preset),
    -- Kept in lockstep with domain.MediaPresets + MediaPresetOriginal.
    CONSTRAINT media_variants_preset_check
        CHECK (preset IN ('original', 'thumb', 'small', 'medium', 'large')),
    -- Kept in lockstep with domain.MediaVariant* states.
    CONSTRAINT media_variants_state_check
        CHECK (state IN ('pending', 'done', 'skipped', 'failed')),
    CONSTRAINT media_variants_attempts_check
        CHECK (attempts >= 0),
    CONSTRAINT media_variants_dims_check
        CHECK ((width_px IS NULL) = (height_px IS NULL)
               AND (width_px IS NULL OR (width_px > 0 AND height_px > 0)))
);

-- The worker's scan: "which pending rows are due". Partial, so a bucket of a
-- million done rows costs nothing to walk past. NULLS FIRST because NULL means
-- "due now" and must sort ahead of every explicit backoff timestamp.
CREATE INDEX media_variants_pending_idx
    ON media_variants (next_attempt_at NULLS FIRST)
    WHERE state = 'pending';

-- Delete and DTO both walk one asset's rows.
CREATE INDEX media_variants_asset_idx
    ON media_variants (asset_id);

ALTER TABLE media_variants ENABLE ROW LEVEL SECURITY;
ALTER TABLE media_variants FORCE ROW LEVEL SECURITY;

-- Tenant scope: the ordinary path. Every read and write from a request, and
-- every per-row worker transaction (which runs under WithTx(tenantID)), goes
-- through this policy.
DROP POLICY IF EXISTS media_variants_tenant_select ON media_variants;
CREATE POLICY media_variants_tenant_select ON media_variants
    FOR SELECT USING (tenant_id = app_current_tenant());
DROP POLICY IF EXISTS media_variants_tenant_insert ON media_variants;
CREATE POLICY media_variants_tenant_insert ON media_variants
    FOR INSERT WITH CHECK (tenant_id = app_current_tenant());
DROP POLICY IF EXISTS media_variants_tenant_update ON media_variants;
CREATE POLICY media_variants_tenant_update ON media_variants
    FOR UPDATE USING (tenant_id = app_current_tenant())
    WITH CHECK (tenant_id = app_current_tenant());
DROP POLICY IF EXISTS media_variants_tenant_delete ON media_variants;
CREATE POLICY media_variants_tenant_delete ON media_variants
    FOR DELETE USING (tenant_id = app_current_tenant());

-- Worker scan: the one CROSS-TENANT read this table allows. Modelled on
-- app_scheduler_scan() (000041), and — as that migration insists — with its
-- own purpose-specific GUC rather than reusing the scheduler's, so that
-- granting one worker a cross-tenant scan never silently grants the other.
-- SELECT only: every UPDATE happens inside WithTx(tenantID) under the tenant
-- policy above, so the scan can locate work but never change it.
CREATE OR REPLACE FUNCTION app_media_transform_scan() RETURNS BOOLEAN
    LANGUAGE sql STABLE AS $$
    SELECT current_setting('app.media_transform_scan', true) = 'on'
$$;

DROP POLICY IF EXISTS media_variants_worker_scan ON media_variants;
CREATE POLICY media_variants_worker_scan ON media_variants
    FOR SELECT USING (app_media_transform_scan());
