-- Reverse of 000044 (ADR-020).
--
-- WHAT IS LOST: every component definition (content_components and their
-- sub-field rows) and every content-type field of type 'component'. The
-- inline VALUES those fields held stay in entries.payload /
-- entries.published_payload as keys no field defines; pruneUndefined drops
-- them on the next write, and nothing reads them before then. A tenant that
-- needs the definitions back must re-import an exported artifact taken
-- before this ran.

DROP FUNCTION IF EXISTS content_component_has_subkey(JSONB, TEXT, TEXT);
DROP FUNCTION IF EXISTS content_component_rewrite_subkey(JSONB, TEXT, TEXT, TEXT);
DROP FUNCTION IF EXISTS content_component_rewrite_item(JSONB, TEXT, TEXT);

-- Referencing fields first (they RESTRICT the components), then sub-fields,
-- then the definitions themselves.
DELETE FROM content_type_fields WHERE field_type = 'component';
DELETE FROM content_type_fields WHERE component_id IS NOT NULL;

ALTER TABLE content_type_fields
    DROP CONSTRAINT IF EXISTS content_type_fields_field_type_check_v3;
ALTER TABLE content_type_fields
    ADD CONSTRAINT content_type_fields_field_type_check_v2
    CHECK (field_type IN (
        'string', 'text', 'richtext', 'number', 'boolean', 'enum', 'date', 'datetime', 'file', 'relation'
    ));

DROP INDEX IF EXISTS content_type_fields_ref_component_idx;
DROP INDEX IF EXISTS content_type_fields_component_key_idx;
ALTER TABLE content_type_fields
    DROP CONSTRAINT IF EXISTS content_type_fields_component_nesting_check;
ALTER TABLE content_type_fields
    DROP CONSTRAINT IF EXISTS content_type_fields_component_ref_check;
ALTER TABLE content_type_fields
    DROP CONSTRAINT IF EXISTS content_type_fields_owner_check;
ALTER TABLE content_type_fields DROP COLUMN IF EXISTS ref_component_id;
ALTER TABLE content_type_fields DROP COLUMN IF EXISTS component_id;
ALTER TABLE content_type_fields
    ALTER COLUMN content_type_id SET NOT NULL;

DROP POLICY IF EXISTS content_components_tenant_isolation ON content_components;
DROP TABLE IF EXISTS content_components;
