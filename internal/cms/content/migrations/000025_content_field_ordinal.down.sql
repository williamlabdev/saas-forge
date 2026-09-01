-- Dropping the column returns field ordering to the tie described in the up
-- migration: same-timestamp fields fall back on whatever the plan produces. The
-- data is not lost so much as it stops being expressible.

ALTER TABLE content_type_fields
    DROP COLUMN IF EXISTS ordinal;
