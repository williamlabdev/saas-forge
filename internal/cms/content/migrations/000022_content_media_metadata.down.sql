-- Reverses 000022. The asset rows and their bytes survive; only the declared
-- metadata goes, and that loss is unrecoverable in a way the other media columns
-- are not: size_bytes and content_type could be re-derived by re-Stat-ing the
-- bucket, but nothing in the object ever carried the original filename or the
-- editor's alt text. Re-applying 000022 returns the columns, not the values.
--
-- Constraints are dropped before the columns they reference so this file is also
-- safe to run against a database where a later migration has since re-shaped one
-- of them.

ALTER TABLE media_assets
    DROP CONSTRAINT IF EXISTS media_assets_dimensions_check;
ALTER TABLE media_assets
    DROP CONSTRAINT IF EXISTS media_assets_filename_check;
ALTER TABLE media_assets
    DROP CONSTRAINT IF EXISTS media_assets_alt_text_check;

ALTER TABLE media_assets
    DROP COLUMN IF EXISTS height_px,
    DROP COLUMN IF EXISTS width_px,
    DROP COLUMN IF EXISTS alt_text,
    DROP COLUMN IF EXISTS filename;
