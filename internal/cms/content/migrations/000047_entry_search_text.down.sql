DROP INDEX IF EXISTS idx_entries_published_search_text_trgm;
DROP INDEX IF EXISTS idx_entries_search_text_trgm;

ALTER TABLE entries
    DROP COLUMN IF EXISTS published_search_text,
    DROP COLUMN IF EXISTS search_text;

DROP FUNCTION IF EXISTS content_entry_search_text_approx(JSONB);

-- The extension is left in place deliberately: dropping it here would also
-- drop pg_trgm's operator classes out from under any OTHER migration that
-- comes to depend on them later, and CREATE EXTENSION IF NOT EXISTS in the up
-- migration is itself idempotent and harmless to leave installed. A tenant
-- database that never runs this migration's up side again never asked for it
-- either way.
