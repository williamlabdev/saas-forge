-- Reusable components (ADR-020).
--
-- A component is a named, tenant-scoped group of fields that a content type
-- field of type 'component' references. Entry VALUES stay inline in the JSONB
-- payload (one object, or an array of objects when the field is multiple);
-- this migration adds only the DEFINITIONS and the helpers the schema-change
-- verbs need to rewrite those inline values set-wise.
--
-- Sub-fields reuse content_type_fields rather than a parallel table, so every
-- attribute a field can carry (type, required, multiple, enum_values,
-- relation_entity, format, pattern, min, max) is stored and validated once.
-- The price is one nullable column: content_type_id may now be NULL, and a
-- CHECK insists that EXACTLY ONE of content_type_id / component_id is set.
-- Every existing query over the table keys on content_type_id (verified in
-- the PR that ships this), so sub-field rows are invisible to them.
--
-- The field_type CHECK is replaced under a NEW name (_v3) for the reason
-- 000028 gave: the rollback guard tracks catalog objects by name, and a
-- versioned name makes both directions of this migration visible to it.
-- (000045 supersedes this constraint again, under _v4; TestFieldTypeSQLParity
-- reads the LATEST such migration, which is now that one.)

CREATE TABLE IF NOT EXISTS content_components (
    id         UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id  TEXT NOT NULL,
    name       TEXT NOT NULL,
    label      TEXT NOT NULL DEFAULT '',
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    CONSTRAINT content_components_tenant_name_key UNIQUE (tenant_id, name)
);

-- Same two-layer isolation as every content table (000014): the app scopes by
-- tenant_id, RLS refuses what a forgotten WHERE would leak. FORCE so the owner
-- is subject too.
ALTER TABLE content_components ENABLE ROW LEVEL SECURITY;
ALTER TABLE content_components FORCE ROW LEVEL SECURITY;
DROP POLICY IF EXISTS content_components_tenant_isolation ON content_components;
CREATE POLICY content_components_tenant_isolation ON content_components
    USING (tenant_id = app_current_tenant())
    WITH CHECK (tenant_id = app_current_tenant());

-- Sub-field rows: content_type_id NULL, component_id = the owning component.
-- Referencing rows (field_type = 'component'): ref_component_id = the
-- component whose shape the value takes. Both FKs RESTRICT: a component in
-- use cannot be dropped out from under its referrers by a cascade, and the
-- service's DELETE /components/{name} answers 409 CONTENT_COMPONENT_IN_USE
-- before the database ever has to.
ALTER TABLE content_type_fields
    ALTER COLUMN content_type_id DROP NOT NULL;
ALTER TABLE content_type_fields
    ADD COLUMN IF NOT EXISTS component_id UUID
        REFERENCES content_components(id) ON DELETE RESTRICT;
ALTER TABLE content_type_fields
    ADD COLUMN IF NOT EXISTS ref_component_id UUID
        REFERENCES content_components(id) ON DELETE RESTRICT;

ALTER TABLE content_type_fields
    ADD CONSTRAINT content_type_fields_owner_check
    CHECK (num_nonnulls(content_type_id, component_id) = 1);
-- A 'component' field always names its component, nothing else ever does.
ALTER TABLE content_type_fields
    ADD CONSTRAINT content_type_fields_component_ref_check
    CHECK ((field_type = 'component') = (ref_component_id IS NOT NULL));
-- No nesting in v1 (ADR-020 §2): a sub-field cannot itself be a component.
ALTER TABLE content_type_fields
    ADD CONSTRAINT content_type_fields_component_nesting_check
    CHECK (NOT (component_id IS NOT NULL AND field_type = 'component'));

-- (content_type_id, key) is unique already; NULLs are distinct there, so
-- sub-fields need their own uniqueness per component.
CREATE UNIQUE INDEX IF NOT EXISTS content_type_fields_component_key_idx
    ON content_type_fields (component_id, key) WHERE component_id IS NOT NULL;
CREATE INDEX IF NOT EXISTS content_type_fields_ref_component_idx
    ON content_type_fields (ref_component_id) WHERE ref_component_id IS NOT NULL;

ALTER TABLE content_type_fields
    DROP CONSTRAINT IF EXISTS content_type_fields_field_type_check_v2;
ALTER TABLE content_type_fields
    ADD CONSTRAINT content_type_fields_field_type_check_v3
    CHECK (field_type IN (
        'string', 'text', 'richtext', 'number', 'boolean', 'enum', 'date', 'datetime', 'file', 'relation', 'component'
    ));

-- Helpers for the sub-field schema-change verbs (rename / delete), which
-- must rewrite the inline value inside EVERY item of EVERY entry of EVERY
-- referencing type, in both payload copies, in one statement per copy. They
-- are IMMUTABLE functions of their arguments so the planner may inline them,
-- and they live in the migration so the SQL in the repository stays a plain
-- UPDATE mirroring RenameField / DeleteField.
--
--   content_component_rewrite_item(item, old_key, new_key)
--       one object: renames old_key to new_key, or drops it when new_key IS NULL.
--   content_component_rewrite_subkey(doc, key, old_key, new_key)
--       the entry document: applies the above under doc->key, whether that is
--       one object or an array of them; a missing key leaves doc untouched.
--   content_component_has_subkey(doc, key, sub_key)
--       the predicate the version-bump and revision CASE guards use: "does
--       any item under doc->key carry sub_key".
CREATE OR REPLACE FUNCTION content_component_rewrite_item(item JSONB, old_key TEXT, new_key TEXT)
RETURNS JSONB LANGUAGE sql IMMUTABLE AS $$
    SELECT CASE
        WHEN jsonb_typeof(item) <> 'object' OR NOT (item ? old_key) THEN item
        WHEN new_key IS NULL THEN item - old_key
        ELSE (item - old_key) || jsonb_build_object(new_key, item -> old_key)
    END
$$;

CREATE OR REPLACE FUNCTION content_component_rewrite_subkey(doc JSONB, key TEXT, old_key TEXT, new_key TEXT)
RETURNS JSONB LANGUAGE sql IMMUTABLE AS $$
    SELECT CASE jsonb_typeof(doc -> key)
        WHEN 'object' THEN
            jsonb_set(doc, ARRAY[key], content_component_rewrite_item(doc -> key, old_key, new_key))
        WHEN 'array' THEN
            jsonb_set(doc, ARRAY[key], COALESCE((
                SELECT jsonb_agg(content_component_rewrite_item(e, old_key, new_key) ORDER BY ord)
                FROM jsonb_array_elements(doc -> key) WITH ORDINALITY AS t(e, ord)
            ), '[]'::jsonb))
        ELSE doc
    END
$$;

CREATE OR REPLACE FUNCTION content_component_has_subkey(doc JSONB, key TEXT, sub_key TEXT)
RETURNS BOOLEAN LANGUAGE sql IMMUTABLE AS $$
    SELECT CASE jsonb_typeof(doc -> key)
        WHEN 'object' THEN (doc -> key) ? sub_key
        WHEN 'array' THEN EXISTS (
            SELECT 1 FROM jsonb_array_elements(doc -> key) AS e
            WHERE jsonb_typeof(e) = 'object' AND e ? sub_key
        )
        ELSE FALSE
    END
$$;
