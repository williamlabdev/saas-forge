-- Reverse references (ADR-024, 2.8a): "what points at this entry".
--
-- entry_relations mirrors entry_media's shape (000019) for the same reason
-- entry_media exists at all: answering "who references X" by scanning every
-- entry's payload at request time has no index to use and needs the field's
-- schema in hand just to know where to look (ADR-024 §Alternatives, and
-- 000019's own header makes the identical argument for media). Maintaining
-- the link on write makes it a keyed lookup instead.
--
-- Two departures from entry_media's exact shape, both explained in ADR-024:
--
--   1. The primary key is (entry_id, field_path, target_entry_id), not
--      (entry_id, asset_id). A relation field is per-FIELD, not per-entry: an
--      entry can legitimately hold two relation fields — "author" and
--      "reviewer" — that name the SAME target, and both facts must survive
--      independently or "which field referenced this" becomes unanswerable.
--      field_path also lets the referenced-by endpoint report the field, and
--      lets a field delete (relinkEntryRelations) drop exactly that field's
--      rows without touching a sibling field that points at the same target.
--
--   2. target_entry_id carries NO foreign key at all — not CASCADE, not
--      RESTRICT. A relation VALUE is a UUID sitting in JSONB; the database has
--      never enforced that it names a live row (checkRelationsIn does, but
--      only at the moment the REFERRING entry is written). entry_relations is
--      a maintained INDEX of that already-unenforced value, not a new
--      invariant on it — so it must not become stricter than the data it
--      indexes. A FK would force one of two bad shapes: RESTRICT would block
--      deleting any entry that happens to be referenced (a category used by
--      ten thousand posts could never be removed), and CASCADE would delete
--      the referencing rows out from under the referrer's payload, which
--      would still hold the now-dangling id — the index would then say "not
--      referenced" while the payload still is. No FK means the row simply
--      goes stale exactly the way the payload's own copy of the id already
--      can: until the referencing entry is next written (ReplaceEntryRelations
--      rebuilds its rows from the current payload), a target's deletion is
--      not reflected in entry_relations. That is the same staleness window
--      entry_media accepts for an orphaned asset, applied to the other side
--      of the pointer.

CREATE TABLE IF NOT EXISTS entry_relations (
    entry_id        UUID NOT NULL REFERENCES entries(id) ON DELETE CASCADE,
    field_path      TEXT NOT NULL,
    target_entry_id UUID NOT NULL,
    tenant_id       TEXT NOT NULL,
    created_at      TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    PRIMARY KEY (entry_id, field_path, target_entry_id)
);

-- The reverse lookup direction: given a target entry, find its referrers.
CREATE INDEX IF NOT EXISTS idx_entry_relations_target
    ON entry_relations (tenant_id, target_entry_id);

-- entry_relations_published is the published-snapshot twin, written only at
-- publish time and left alone on retract — same split, same reasoning, as
-- entry_media_published (000020): a reference held only by a draft must not
-- appear as if the public-facing snapshot depends on it.
CREATE TABLE IF NOT EXISTS entry_relations_published (
    entry_id        UUID NOT NULL REFERENCES entries(id) ON DELETE CASCADE,
    field_path      TEXT NOT NULL,
    target_entry_id UUID NOT NULL,
    tenant_id       TEXT NOT NULL,
    created_at      TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    PRIMARY KEY (entry_id, field_path, target_entry_id)
);

CREATE INDEX IF NOT EXISTS idx_entry_relations_published_target
    ON entry_relations_published (tenant_id, target_entry_id);

ALTER TABLE entry_relations ENABLE ROW LEVEL SECURITY;
ALTER TABLE entry_relations FORCE ROW LEVEL SECURITY;
DROP POLICY IF EXISTS entry_relations_tenant_isolation ON entry_relations;
CREATE POLICY entry_relations_tenant_isolation ON entry_relations
    USING (tenant_id = app_current_tenant())
    WITH CHECK (tenant_id = app_current_tenant());

ALTER TABLE entry_relations_published ENABLE ROW LEVEL SECURITY;
ALTER TABLE entry_relations_published FORCE ROW LEVEL SECURITY;
DROP POLICY IF EXISTS entry_relations_published_tenant_isolation ON entry_relations_published;
CREATE POLICY entry_relations_published_tenant_isolation ON entry_relations_published
    USING (tenant_id = app_current_tenant())
    WITH CHECK (tenant_id = app_current_tenant());

-- --- backfill --------------------------------------------------------------
--
-- Existing entries hold relation values with no entry_relations rows at all
-- (the table did not exist when they were written). 000047's search_text
-- backfill is NOT the template here, deliberately: search_text's
-- content_entry_search_text_approx walks the JSON tree SCHEMA-BLIND (any
-- string under `.text`/`.alt`/`.code`, or any top-level string), which is
-- fine for a search index where a false positive just adds a word nobody
-- searches for. A relation value is a bare UUID string — structurally
-- IDENTICAL to a plain string/enum field's value — so a schema-blind scan
-- would insert reference rows for entries that merely happen to contain a
-- UUID-shaped string in an unrelated field. That is a correctness bug for a
-- table whose whole job is "what points at this entry", not a cosmetic one.
--
-- This backfill is schema-AWARE instead, built from content_type_fields (the
-- same catalog checkRelationsIn/mediaRefsIn read at request time) rather than
-- the payload's shape alone. It runs inside this migration's own transaction,
-- as the DB owner — migrations are not RLS-scoped (see 000020's cross-tenant
-- backfill for the same property), which is what lets one statement seed
-- every tenant at once.
--
-- relation_field_paths enumerates every place a relation value can sit, one
-- level of nesting deep (ADR-020 §2: a sub-field cannot itself be a component
-- or dynamic zone, so three cases — top-level, component-embedded,
-- dynamiczone-embedded — are exhaustive):
--
--   field_path mirrors what prefixed() writes at request time ("key" for a
--   top-level field, "parent.key" for a component or zone sub-field), so a
--   row this backfill inserts and a row ReplaceEntryRelations inserts later
--   for the same value are byte-identical and collide harmlessly on
--   ON CONFLICT DO NOTHING rather than duplicating.
--
--   jsonpath mirrors mediaPaths' construction (internal/cms/content/repository
--   /postgres_repository.go): a component's items are reached with `[*]`,
--   which in lax mode wraps a single object as well as iterating a list, and
--   a dynamic zone's items are additionally filtered to the ones tagged with
--   the naming component (`__component`). A `multiple` relation field adds
--   its own trailing `[*]` to unwrap the array of ids the way a scalar
--   relation field does not need to.
WITH relation_field_paths AS (
    SELECT
        ct.id AS content_type_id,
        f.key AS field_path,
        CASE WHEN f.multiple
             THEN format('$."%s"[*]', f.key)
             ELSE format('$."%s"', f.key)
        END AS jsonpath
    FROM content_type_fields f
    JOIN content_types ct ON ct.id = f.content_type_id
    WHERE f.field_type = 'relation'

    UNION ALL

    SELECT
        ct.id,
        format('%s.%s', pf.key, sf.key),
        format('$."%s"[*]."%s"%s', pf.key, sf.key, CASE WHEN sf.multiple THEN '[*]' ELSE '' END)
    FROM content_type_fields pf
    JOIN content_types ct ON ct.id = pf.content_type_id
    JOIN content_type_fields sf ON sf.component_id = pf.ref_component_id
    WHERE pf.field_type = 'component' AND sf.field_type = 'relation'

    UNION ALL

    SELECT
        ct.id,
        format('%s.%s', zf.key, sf.key),
        format('$."%s"[*] ? (@."__component" == "%s")."%s"%s',
               zf.key, c.name, sf.key, CASE WHEN sf.multiple THEN '[*]' ELSE '' END)
    FROM content_type_fields zf
    JOIN content_types ct ON ct.id = zf.content_type_id
    -- Component names are unique per tenant, not globally (content_components
    -- has no cross-tenant uniqueness), so the join back to the zone's OWN
    -- tenant is load-bearing: without it, a zone in tenant A that allows a
    -- component named "hero" would also match tenant B's unrelated "hero".
    JOIN content_components c ON c.name = ANY(zf.zone_components) AND c.tenant_id = ct.tenant_id
    JOIN content_type_fields sf ON sf.component_id = c.id AND sf.field_type = 'relation'
    WHERE zf.field_type = 'dynamiczone'
)
INSERT INTO entry_relations (entry_id, field_path, target_entry_id, tenant_id, created_at)
SELECT DISTINCT e.id, rfp.field_path, (val #>> '{}')::uuid, e.tenant_id, NOW()
FROM entries e
JOIN relation_field_paths rfp ON rfp.content_type_id = e.content_type_id
CROSS JOIN LATERAL jsonb_path_query(e.payload, rfp.jsonpath::jsonpath, '{}'::jsonb, true) AS val
-- jsonb_path_query runs silent on a value of the wrong shape (see
-- relinkEntryMedia's identical comment), so a field whose stored value is
-- somehow not a string yields nothing rather than failing the backfill. The
-- uuid-format guard is the schema-aware backfill's OWN safety net beyond
-- that: relation_field_paths already restricts WHICH keys are read, but a
-- value that reached storage before some later validation tightening could
-- still be a non-uuid string, and this must not fail the migration over it.
WHERE jsonb_typeof(val) = 'string'
  AND (val #>> '{}') ~ '^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$'
ON CONFLICT DO NOTHING;

-- Published twin: the same extraction over published_payload, restricted to
-- entries that actually have one (status = 'published' implies it by the
-- entries_published_snapshot_check constraint from 000020).
WITH relation_field_paths AS (
    SELECT
        ct.id AS content_type_id,
        f.key AS field_path,
        CASE WHEN f.multiple
             THEN format('$."%s"[*]', f.key)
             ELSE format('$."%s"', f.key)
        END AS jsonpath
    FROM content_type_fields f
    JOIN content_types ct ON ct.id = f.content_type_id
    WHERE f.field_type = 'relation'

    UNION ALL

    SELECT
        ct.id,
        format('%s.%s', pf.key, sf.key),
        format('$."%s"[*]."%s"%s', pf.key, sf.key, CASE WHEN sf.multiple THEN '[*]' ELSE '' END)
    FROM content_type_fields pf
    JOIN content_types ct ON ct.id = pf.content_type_id
    JOIN content_type_fields sf ON sf.component_id = pf.ref_component_id
    WHERE pf.field_type = 'component' AND sf.field_type = 'relation'

    UNION ALL

    SELECT
        ct.id,
        format('%s.%s', zf.key, sf.key),
        format('$."%s"[*] ? (@."__component" == "%s")."%s"%s',
               zf.key, c.name, sf.key, CASE WHEN sf.multiple THEN '[*]' ELSE '' END)
    FROM content_type_fields zf
    JOIN content_types ct ON ct.id = zf.content_type_id
    JOIN content_components c ON c.name = ANY(zf.zone_components) AND c.tenant_id = ct.tenant_id
    JOIN content_type_fields sf ON sf.component_id = c.id AND sf.field_type = 'relation'
    WHERE zf.field_type = 'dynamiczone'
)
INSERT INTO entry_relations_published (entry_id, field_path, target_entry_id, tenant_id, created_at)
SELECT DISTINCT e.id, rfp.field_path, (val #>> '{}')::uuid, e.tenant_id, NOW()
FROM entries e
JOIN relation_field_paths rfp ON rfp.content_type_id = e.content_type_id
CROSS JOIN LATERAL jsonb_path_query(e.published_payload, rfp.jsonpath::jsonpath, '{}'::jsonb, true) AS val
WHERE e.status = 'published'
  AND jsonb_typeof(val) = 'string'
  AND (val #>> '{}') ~ '^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$'
ON CONFLICT DO NOTHING;
