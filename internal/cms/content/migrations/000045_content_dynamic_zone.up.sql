-- Dynamic zones (ADR-020 Amendment 1).
--
-- A dynamic zone is a content-type field whose value is an ORDERED list of
-- component items of DIFFERENT components — the page-builder shape a
-- repeatable single-component field cannot express. The field declares which
-- components it accepts (zone_components); each item names the one it is,
-- inline in the payload, under the key '__component'.
--
-- Nothing new is stored per item: zone items reuse the SAME sub-field rows
-- (component_id) 000044 introduced, so every sub-field attribute is still
-- stored and validated once, and every query that walks a component's
-- sub-fields reaches a zone's items for free.
--
-- The allowed-list stores NAMES, not ids, and therefore carries no FK. That
-- is deliberate: an item names its component in the payload, so the list and
-- the items must speak one alphabet — a rename has to rewrite both, and it
-- does, in one transaction. The price is that "this component is still
-- referenced by a zone" is not a constraint the database can state; the
-- service checks it inside the delete's own transaction and answers 409
-- CONTENT_COMPONENT_IN_USE.
--
-- The field_type CHECK is replaced under a NEW name (_v4) for the reason
-- 000028 and 000044 gave: the rollback guard tracks catalog objects by name,
-- and a versioned name makes both directions visible to it.
-- TestFieldTypeSQLParity now reads THIS file as the field_type contract.

ALTER TABLE content_type_fields
    ADD COLUMN IF NOT EXISTS zone_components TEXT[];

-- A dynamic zone always declares at least one component; nothing else ever
-- carries a list. Both halves matter: an empty list would be a field that can
-- hold nothing but refuses every item with "not in the allowed list", and a
-- list on a 'string' field would be a shape no reader knows to look at.
ALTER TABLE content_type_fields
    ADD CONSTRAINT content_type_fields_zone_components_check
    CHECK ((field_type = 'dynamiczone')
           = (zone_components IS NOT NULL AND cardinality(zone_components) > 0));

-- No nesting, exactly as for 'component' (ADR-020 §2): a sub-field cannot be
-- a zone either. This is a SEPARATE constraint rather than a wider version of
-- 000044's nesting check, so that this migration's down direction is an exact
-- reverse — re-adding a constraint 000044 owns would leave the catalog in a
-- state neither migration describes.
ALTER TABLE content_type_fields
    ADD CONSTRAINT content_type_fields_zone_nesting_check
    CHECK (NOT (component_id IS NOT NULL AND field_type = 'dynamiczone'));

ALTER TABLE content_type_fields
    DROP CONSTRAINT IF EXISTS content_type_fields_field_type_check_v3;
ALTER TABLE content_type_fields
    ADD CONSTRAINT content_type_fields_field_type_check_v4
    CHECK (field_type IN (
        'string', 'text', 'richtext', 'number', 'boolean', 'enum', 'date', 'datetime', 'file', 'relation', 'component', 'dynamiczone'
    ));

-- Helpers for the schema-change verbs, alongside 000044's. A zone's value is
-- always an ARRAY, and its items are heterogeneous, so every one of these
-- takes the component name to act on and leaves the other items alone. They
-- are IMMUTABLE functions of their arguments so the planner may inline them,
-- and a NULL or non-array value under key is a no-op — which is what makes
-- them safe to run over a NULL published_payload.
--
--   content_zone_rewrite_component(doc, key, old_name, new_name)
--       component rename: retags every item that names old_name.
--   content_zone_has_component(doc, key, name)
--       "does any item under doc->key name this component" — the predicate
--       behind both the rename's CASE guard and the delete's 409.
--   content_zone_rewrite_subkey(doc, key, comp_name, old_key, new_key)
--       sub-field rename (new_key NULL = delete), applied only to the items
--       of comp_name; 000044's rewrite_item does the per-item work, so the
--       two paths cannot drift on what "rename a sub-field" means.
--   content_zone_has_subkey(doc, key, comp_name, sub_key)
--       the CASE-guard predicate for the above.
CREATE OR REPLACE FUNCTION content_zone_rewrite_component(doc JSONB, key TEXT, old_name TEXT, new_name TEXT)
RETURNS JSONB LANGUAGE sql IMMUTABLE AS $$
    SELECT CASE jsonb_typeof(doc -> key)
        WHEN 'array' THEN
            jsonb_set(doc, ARRAY[key], COALESCE((
                SELECT jsonb_agg(
                    CASE
                        WHEN jsonb_typeof(e) = 'object' AND e ->> '__component' = old_name
                        THEN e || jsonb_build_object('__component', new_name)
                        ELSE e
                    END ORDER BY ord)
                FROM jsonb_array_elements(doc -> key) WITH ORDINALITY AS t(e, ord)
            ), '[]'::jsonb))
        ELSE doc
    END
$$;

CREATE OR REPLACE FUNCTION content_zone_has_component(doc JSONB, key TEXT, name TEXT)
RETURNS BOOLEAN LANGUAGE sql IMMUTABLE AS $$
    SELECT CASE jsonb_typeof(doc -> key)
        WHEN 'array' THEN EXISTS (
            SELECT 1 FROM jsonb_array_elements(doc -> key) AS e
            WHERE jsonb_typeof(e) = 'object' AND e ->> '__component' = name
        )
        ELSE FALSE
    END
$$;

CREATE OR REPLACE FUNCTION content_zone_rewrite_subkey(doc JSONB, key TEXT, comp_name TEXT, old_key TEXT, new_key TEXT)
RETURNS JSONB LANGUAGE sql IMMUTABLE AS $$
    SELECT CASE jsonb_typeof(doc -> key)
        WHEN 'array' THEN
            jsonb_set(doc, ARRAY[key], COALESCE((
                SELECT jsonb_agg(
                    CASE
                        WHEN jsonb_typeof(e) = 'object' AND e ->> '__component' = comp_name
                        THEN content_component_rewrite_item(e, old_key, new_key)
                        ELSE e
                    END ORDER BY ord)
                FROM jsonb_array_elements(doc -> key) WITH ORDINALITY AS t(e, ord)
            ), '[]'::jsonb))
        ELSE doc
    END
$$;

CREATE OR REPLACE FUNCTION content_zone_has_subkey(doc JSONB, key TEXT, comp_name TEXT, sub_key TEXT)
RETURNS BOOLEAN LANGUAGE sql IMMUTABLE AS $$
    SELECT CASE jsonb_typeof(doc -> key)
        WHEN 'array' THEN EXISTS (
            SELECT 1 FROM jsonb_array_elements(doc -> key) AS e
            WHERE jsonb_typeof(e) = 'object' AND e ->> '__component' = comp_name AND e ? sub_key
        )
        ELSE FALSE
    END
$$;
