-- Reverse of 000045 (ADR-020 Amendment 1).
--
-- WHAT IS LOST: every dynamic-zone field definition and its allowed-list. The
-- inline item ARRAYS those fields held stay in entries.payload /
-- entries.published_payload as keys no field defines; pruneUndefined drops
-- them on the next write, and nothing reads them before then — the same
-- bargain 000044 struck for 'component' fields. The components themselves and
-- their sub-field rows are untouched: this migration never owned them.

DROP FUNCTION IF EXISTS content_zone_has_subkey(JSONB, TEXT, TEXT, TEXT);
DROP FUNCTION IF EXISTS content_zone_rewrite_subkey(JSONB, TEXT, TEXT, TEXT, TEXT);
DROP FUNCTION IF EXISTS content_zone_has_component(JSONB, TEXT, TEXT);
DROP FUNCTION IF EXISTS content_zone_rewrite_component(JSONB, TEXT, TEXT, TEXT);

-- Before the narrower CHECK goes back on, or it fails against the rows it
-- would now forbid.
DELETE FROM content_type_fields WHERE field_type = 'dynamiczone';

ALTER TABLE content_type_fields
    DROP CONSTRAINT IF EXISTS content_type_fields_field_type_check_v4;
ALTER TABLE content_type_fields
    ADD CONSTRAINT content_type_fields_field_type_check_v3
    CHECK (field_type IN (
        'string', 'text', 'richtext', 'number', 'boolean', 'enum', 'date', 'datetime', 'file', 'relation', 'component'
    ));

ALTER TABLE content_type_fields
    DROP CONSTRAINT IF EXISTS content_type_fields_zone_nesting_check;
ALTER TABLE content_type_fields
    DROP CONSTRAINT IF EXISTS content_type_fields_zone_components_check;
ALTER TABLE content_type_fields DROP COLUMN IF EXISTS zone_components;
