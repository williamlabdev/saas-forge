-- Multi-locale content (STRATEGY §三 Headless CMS gap 4; ADR-004 sequenced it
-- after the delivery path because it changes the shape of an entry's identity).
--
-- Model: ONE ROW PER LOCALE, related by translation_group_id.
--   - A translation publishes independently — English can be live while the
--     Chinese draft is still being written. That is the requirement a single
--     row with a locale-keyed payload cannot meet, since status is per row.
--   - status / RLS / the optimistic lock / the publish flow are unchanged: the
--     delivery path just gains one more column predicate, exactly like status.
--   - Cost accepted: each locale counts as its own entry against the plan, and
--     "the same article" spans rows, so callers must group by translation_group_id.
--
-- Backfill: every existing row becomes its own translation group in the default
-- locale. No content moves, and single-locale tenants keep behaving as before.

ALTER TABLE entries
    ADD COLUMN IF NOT EXISTS locale               TEXT NOT NULL DEFAULT 'default',
    ADD COLUMN IF NOT EXISTS translation_group_id UUID;

-- Each pre-existing entry is its own group. Done before the NOT NULL so the
-- column can be tightened immediately afterwards.
UPDATE entries SET translation_group_id = id WHERE translation_group_id IS NULL;

-- NOT NULL with a DEFAULT, not NOT NULL alone: "an entry with no stated group
-- is a group of one" is the actual semantics, and encoding it here means an
-- insert that predates localisation (an import, a fixture, a manual fix-up)
-- still lands in a valid state instead of failing on a missing column.
ALTER TABLE entries ALTER COLUMN translation_group_id SET DEFAULT gen_random_uuid();
ALTER TABLE entries ALTER COLUMN translation_group_id SET NOT NULL;

-- A locale tag must be a usable identifier (BCP-47-ish: 'en', 'zh-TW', and the
-- 'default' placeholder for tenants that never opted into localisation).
ALTER TABLE entries DROP CONSTRAINT IF EXISTS entries_locale_check;
ALTER TABLE entries
    ADD CONSTRAINT entries_locale_check
    CHECK (locale ~ '^[a-zA-Z][a-zA-Z0-9_-]{0,34}$');

-- The invariant that makes the group meaningful: one row per locale per group.
-- Without it a group could hold two English rows and "the English version"
-- would be ambiguous.
CREATE UNIQUE INDEX IF NOT EXISTS idx_entries_group_locale
    ON entries (tenant_id, translation_group_id, locale);

-- Delivery reads one locale of one type, newest first. Mirrors the status index
-- added in 000016 — the public path must not scan other locales.
CREATE INDEX IF NOT EXISTS idx_entries_tenant_type_locale_status_created
    ON entries (tenant_id, content_type_id, locale, status, created_at DESC);
