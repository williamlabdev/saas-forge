-- 000047: schema-aware plain-text search columns (ADR-021).
--
-- entries.payload / published_payload are JSONB documents; the only indexes
-- on them are jsonb_path_ops GIN indexes serving `@>` containment (000010,
-- 000039), and the admin list's `filter` contains operator falls back to
-- `(payload ->> key) ILIKE …` per-column, per-request — no index, and, for a
-- richtext field, an ILIKE against the RAW BLOCK JSON rather than its prose
-- (content_service.go's parseFilter comment). Searching "does this entry
-- mention X anywhere" has never had a real answer.
--
-- ADR-021 chose two plain-text columns plus pg_trgm over Postgres's built-in
-- tsvector/tsquery full-text search, for one reason that overrides every
-- usual argument in tsvector's favour: the built-in text search parser does
-- not tokenize Chinese. `to_tsvector('simple', '我愛台灣')` — or any other
-- built-in configuration — treats the whole run of CJK characters as ONE
-- token, so a query for "台灣" alone never matches it; only the exact
-- substring position would. For content whose majority audience is
-- Traditional Chinese, a tsvector index would rank and stem English content
-- well and be silently useless for the language most rows are written in.
-- pg_trgm has no such blind spot — it indexes 3-character sequences
-- regardless of script, so both `ILIKE '%台灣%'` and `ILIKE '%taiwan%'` hit the
-- index the same way.
--
-- search_text / published_search_text hold PLAIN TEXT extracted from a
-- content type's string/text/richtext fields (recursing into component and
-- dynamic zone sub-fields one level, ADR-020 §3 / Amendment 1 §2) — see
-- domain.ExtractSearchText. That extractor needs the schema, which no SQL
-- statement in this migration has access to, so the backfill below and the
-- generated helper function it shares with two bulk-rewrite call sites
-- (DeleteField, rewriteComponentItems in postgres_components.go) use a
-- SCHEMA-BLIND APPROXIMATION instead: every string value found anywhere in
-- the JSON tree, via `$.**.text` / `$.**.alt` / `$.**.code` (richtext spans,
-- image alt text, code blocks) plus the document's own top-level string
-- values (covering plain string/text fields, and one level of component
-- fields whose sub-field values sit directly under a top-level key). This
-- over-matches slightly — e.g. it cannot tell a `string` field from an
-- `enum` field storing prose-shaped text, both being top-level strings — and
-- under-matches nothing domain.ExtractSearchText would find, so backfilled
-- and bulk-rewritten rows are searchable immediately and get replaced with
-- the precise value the next time a person or an API caller writes through
-- CreateLocalizedEntry, UpdateEntry, RestoreEntryRevision, or a publish.
--
-- NOT NULL DEFAULT '': every row must have a comparable value for ILIKE, and
-- '' is what "no searchable text" already means to an ILIKE '%term%' clause
-- (never matches a non-empty term, exactly as intended).
--
-- Not CONCURRENTLY, matching 000039: the ledger applies each migration in a
-- transaction, which CREATE INDEX CONCURRENTLY refuses.
CREATE EXTENSION IF NOT EXISTS pg_trgm;

ALTER TABLE entries
    ADD COLUMN IF NOT EXISTS search_text TEXT NOT NULL DEFAULT '',
    ADD COLUMN IF NOT EXISTS published_search_text TEXT NOT NULL DEFAULT '';

-- content_entry_search_text_approx is the one place the approximation formula
-- above is written down; the backfill below and the two bulk SQL-only rewrite
-- sites (DeleteField's bulk strip, rewriteComponentItems's delete path) all
-- call it rather than repeating the jsonb_path_query expression, so the three
-- call sites cannot drift out of sync with each other.
CREATE OR REPLACE FUNCTION content_entry_search_text_approx(doc JSONB)
RETURNS TEXT
LANGUAGE sql
IMMUTABLE
PARALLEL SAFE
AS $$
    SELECT COALESCE(
        (
            SELECT string_agg(v, E'\n')
            FROM (
                SELECT value #>> '{}' AS v
                FROM jsonb_each(doc)
                WHERE jsonb_typeof(value) = 'string'
                UNION ALL
                SELECT value #>> '{}' AS v
                FROM jsonb_path_query(doc, '$.**.text') AS value
                WHERE jsonb_typeof(value) = 'string'
                UNION ALL
                SELECT value #>> '{}' AS v
                FROM jsonb_path_query(doc, '$.**.alt') AS value
                WHERE jsonb_typeof(value) = 'string'
                UNION ALL
                SELECT value #>> '{}' AS v
                FROM jsonb_path_query(doc, '$.**.code') AS value
                WHERE jsonb_typeof(value) = 'string'
            ) parts
            WHERE v <> ''
        ),
        ''
    );
$$;

UPDATE entries
SET search_text = content_entry_search_text_approx(payload)
WHERE payload IS NOT NULL AND payload <> '{}'::jsonb;

UPDATE entries
SET published_search_text = content_entry_search_text_approx(published_payload)
WHERE published_payload IS NOT NULL AND published_payload <> '{}'::jsonb;

-- gin_trgm_ops, not the bare column: pg_trgm's opclass is what makes ILIKE
-- '%term%' able to use the index at all (a plain btree cannot serve a
-- leading-wildcard pattern).
CREATE INDEX IF NOT EXISTS idx_entries_search_text_trgm
    ON entries USING GIN (search_text gin_trgm_ops);

CREATE INDEX IF NOT EXISTS idx_entries_published_search_text_trgm
    ON entries USING GIN (published_search_text gin_trgm_ops);
