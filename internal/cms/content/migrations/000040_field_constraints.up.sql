-- 000040: value-level field constraints — unique, format, pattern, min/max —
-- and the table that makes `unique` a database fact.
--
-- The five attributes ride on content_type_fields as columns rather than on a
-- new field type: a `slug` type would have touched the vendored contract's
-- type enum (four hand-synced copies and a parity test), while a string with
-- format='slug' and is_unique touches nothing a client has to relearn. See
-- domain/field_constraints.go for what each one means and ADR-007 Amendment 1
-- for why they are mutable with guards.
--
-- `unique` is the reserved word, hence is_unique. min/max are DOUBLE PRECISION
-- because they are decoded into float64 on both sides of the wire (a JSON
-- number is one already) and a NUMERIC would only add a conversion.

ALTER TABLE content_type_fields
    ADD COLUMN IF NOT EXISTS is_unique BOOLEAN NOT NULL DEFAULT FALSE,
    ADD COLUMN IF NOT EXISTS format    TEXT    NOT NULL DEFAULT '',
    ADD COLUMN IF NOT EXISTS pattern   TEXT    NOT NULL DEFAULT '',
    ADD COLUMN IF NOT EXISTS min_value DOUBLE PRECISION,
    ADD COLUMN IF NOT EXISTS max_value DOUBLE PRECISION;

-- The closed format set and the range order are the two definition-time rules
-- cheap enough to state twice. The service refuses them by name first; these
-- catch a row written around it.
ALTER TABLE content_type_fields
    DROP CONSTRAINT IF EXISTS content_type_fields_format_check,
    ADD CONSTRAINT content_type_fields_format_check CHECK (format IN ('', 'slug'));
ALTER TABLE content_type_fields
    DROP CONSTRAINT IF EXISTS content_type_fields_range_check,
    ADD CONSTRAINT content_type_fields_range_check
        CHECK (min_value IS NULL OR max_value IS NULL OR min_value <= max_value);

-- entry_unique_values is the reservation ledger behind is_unique: one row per
-- (field, locale, value) naming the entry that holds it. The repository
-- refreshes an entry's rows in the SAME transaction as every write to the
-- entry — create, update, publish, unpublish — from the row it just wrote, so
-- the ledger is derived state that can never disagree with `entries` for
-- longer than a transaction. Both copies reserve: an entry holds a value while
-- its working copy OR its live snapshot (status = 'published') carries it. A
-- retracted snapshot reserves nothing.
--
-- Keyed by field_id, not by key, so RenameField needs no rewrite here and a
-- deleted field takes its reservations with it (CASCADE). Keyed by locale
-- because a translation is a different document with the same slug on
-- purpose. tenant_id is denormalised for RLS and for the same reason every
-- other content table carries it.
--
-- Not a per-field partial unique index on entries: that is DDL in the request
-- path, and CREATE INDEX takes a SHARE lock on the whole entries table — every
-- tenant's writes wait while one tenant ticks a box.
CREATE TABLE IF NOT EXISTS entry_unique_values (
    tenant_id TEXT NOT NULL,
    field_id  UUID NOT NULL REFERENCES content_type_fields(id) ON DELETE CASCADE,
    entry_id  UUID NOT NULL REFERENCES entries(id) ON DELETE CASCADE,
    locale    TEXT NOT NULL,
    value     TEXT NOT NULL,
    PRIMARY KEY (field_id, locale, value)
);
CREATE INDEX IF NOT EXISTS idx_entry_unique_values_entry ON entry_unique_values (entry_id);

-- Same two-layer isolation as every content table (000014): the app scopes by
-- tenant_id, RLS refuses what a forgotten WHERE would leak. FORCE so the owner
-- is subject too. DELETE is granted, unlike 000037: a reservation is released
-- whenever the value it names is no longer held.
ALTER TABLE entry_unique_values ENABLE ROW LEVEL SECURITY;
ALTER TABLE entry_unique_values FORCE ROW LEVEL SECURITY;
DROP POLICY IF EXISTS entry_unique_values_tenant_read ON entry_unique_values;
CREATE POLICY entry_unique_values_tenant_read ON entry_unique_values
    FOR SELECT USING (tenant_id = app_current_tenant());
DROP POLICY IF EXISTS entry_unique_values_tenant_insert ON entry_unique_values;
CREATE POLICY entry_unique_values_tenant_insert ON entry_unique_values
    FOR INSERT WITH CHECK (tenant_id = app_current_tenant());
DROP POLICY IF EXISTS entry_unique_values_tenant_delete ON entry_unique_values;
CREATE POLICY entry_unique_values_tenant_delete ON entry_unique_values
    FOR DELETE USING (tenant_id = app_current_tenant());
