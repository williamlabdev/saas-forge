-- Reverses 000043.
--
-- WHAT IS LOST. Every rendition's bookkeeping: which presets were rendered,
-- where they live, why the others were skipped, and how many times a failing
-- one was retried. The rendition OBJECTS stay in the bucket, now unreferenced
-- by any row — findable only by their `<key>.<preset>.<ext>` suffix, which is
-- why the key scheme was made derivable from the source key (ADR-019). With
-- the table gone, every `?preset=` request serves the original, which is the
-- same thing it did before the rendition was ready: a rollback degrades to the
-- pre-feature behaviour, it does not break a page.
--
-- WHAT SURVIVES. media_assets is untouched; nothing here was ever a column on
-- it. Re-applying 000043 gives an empty queue — existing assets are not
-- re-enqueued by the migration, only by POST /media/{id}/variants.

DROP POLICY IF EXISTS media_variants_worker_scan ON media_variants;
DROP POLICY IF EXISTS media_variants_tenant_delete ON media_variants;
DROP POLICY IF EXISTS media_variants_tenant_update ON media_variants;
DROP POLICY IF EXISTS media_variants_tenant_insert ON media_variants;
DROP POLICY IF EXISTS media_variants_tenant_select ON media_variants;
DROP INDEX IF EXISTS media_variants_asset_idx;
DROP INDEX IF EXISTS media_variants_pending_idx;
DROP TABLE IF EXISTS media_variants;
DROP FUNCTION IF EXISTS app_media_transform_scan();
