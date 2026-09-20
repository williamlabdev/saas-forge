DROP TABLE IF EXISTS entry_unique_values;
ALTER TABLE content_type_fields
    DROP CONSTRAINT IF EXISTS content_type_fields_format_check,
    DROP CONSTRAINT IF EXISTS content_type_fields_range_check;
ALTER TABLE content_type_fields
    DROP COLUMN IF EXISTS is_unique,
    DROP COLUMN IF EXISTS format,
    DROP COLUMN IF EXISTS pattern,
    DROP COLUMN IF EXISTS min_value,
    DROP COLUMN IF EXISTS max_value;
