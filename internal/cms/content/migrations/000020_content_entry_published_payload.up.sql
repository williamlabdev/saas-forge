-- Draft / published CONTENT separation (ADR-006).
--
-- 000016 added an editorial `status`, but both states read the same `payload`
-- column. That made publish deliberate only for an entry's FIRST publish: once
-- status = 'published', every later UPDATE went live immediately, because the
-- delivery path filters on status and then reads the very row the editor just
-- wrote. ADR-004's "deliberate publish" therefore never held for edits to
-- existing content — see ADR-006 for the gap analysis.
--
-- This migration gives published content its own snapshot:
--   payload           — the working copy. Editors write here. Never public.
--   published_payload — what delivery serves. Only publish writes it.
--
-- published_version records WHICH version was snapshotted. It is not how
-- "this entry has unpublished changes" is answered: version bumps on every
-- write, including one that stores the content the row already had, so it
-- reports edits that do not exist. That question is answered by comparing the
-- two payloads — `payload IS DISTINCT FROM published_payload`, a semantic
-- jsonb comparison, so the same content written two ways is not an edit.
-- (An earlier revision of this header claimed the opposite; see ADR-006.)
--
-- Scope limit, stated plainly: this is ONE snapshot, not a version history.
-- 000016's header already noted a version history is "a separate,
-- not-yet-scheduled increment" — that is still true after this migration.

ALTER TABLE entries
    ADD COLUMN IF NOT EXISTS published_payload JSONB,
    ADD COLUMN IF NOT EXISTS published_version INT;

-- Backfill BEFORE the CHECK below, or the constraint fails on existing rows.
-- Already-published rows are live RIGHT NOW with `payload` — seeding the
-- snapshot from anything else would silently change what delivery serves.
UPDATE entries
   SET published_payload = payload,
       published_version = version
 WHERE status = 'published'
   AND published_payload IS NULL;

-- The snapshot exists exactly when the entry is published. Enforced in the DB
-- so no future writer can produce a row that claims published but has nothing
-- to serve (or an unpublished row leaking a stale snapshot). Same shape as
-- entries_published_at_check from 000016.
ALTER TABLE entries
    DROP CONSTRAINT IF EXISTS entries_published_snapshot_check;
ALTER TABLE entries
    ADD CONSTRAINT entries_published_snapshot_check
    CHECK ((status = 'published') = (published_payload IS NOT NULL AND published_version IS NOT NULL));

-- entry_media tracks the WORKING copy's asset references (it is rewritten on
-- every payload write). The delivery gate must not use it: dropping an image
-- from a draft would revoke bytes that the published snapshot still references,
-- breaking a live page. This table is the published counterpart — written only
-- at publish time, cleared on unpublish. See AssetIsPublished.
CREATE TABLE IF NOT EXISTS entry_media_published (
    entry_id  UUID NOT NULL REFERENCES entries(id) ON DELETE CASCADE,
    asset_id  UUID NOT NULL REFERENCES media_assets(id) ON DELETE CASCADE,
    tenant_id TEXT NOT NULL,
    PRIMARY KEY (entry_id, asset_id)
);

-- The delivery lookup direction: given an asset, find its referencing published
-- entries. Mirrors idx_entry_media_asset from 000019.
CREATE INDEX IF NOT EXISTS idx_entry_media_published_asset
    ON entry_media_published (tenant_id, asset_id);

-- Backfill: rows already published had their working refs serving as published
-- refs under the old model, so those are exactly the live references.
INSERT INTO entry_media_published (entry_id, asset_id, tenant_id)
SELECT em.entry_id, em.asset_id, em.tenant_id
  FROM entry_media em
  JOIN entries e ON e.id = em.entry_id
 WHERE e.status = 'published'
ON CONFLICT DO NOTHING;

-- Same tenant isolation as the rest of the content plane (see 000014 / 000019).
ALTER TABLE entry_media_published ENABLE ROW LEVEL SECURITY;
ALTER TABLE entry_media_published FORCE ROW LEVEL SECURITY;
DROP POLICY IF EXISTS entry_media_published_tenant_isolation ON entry_media_published;
CREATE POLICY entry_media_published_tenant_isolation ON entry_media_published
    USING (tenant_id = app_current_tenant())
    WITH CHECK (tenant_id = app_current_tenant());
