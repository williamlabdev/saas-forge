-- Give a field's DEFINITION ORDER a column of its own.
--
-- It has never had one. loadFields ordered by created_at, and CreateContentType
-- stamps every field of a type with ONE timestamp — so for every type ever
-- created in a single call, the ORDER BY has been a total tie, resolved by
-- whatever physical order the plan happened to produce. It matched insertion
-- order in practice, which is why nothing caught it.
--
-- That is not a cosmetic tie. Field order is load-bearing twice over:
--
--   * the admin form renders fields in this order, so a silent reshuffle
--     rearranges a UI nobody edited;
--   * NewArtifact deliberately does NOT sort fields (types are sorted, fields
--     are not) because the order is part of the type's definition — which makes
--     it part of the byte-identical round-trip that ADR-008 exists to provide.
--     An export whose field order depends on the query plan cannot be diffed.
--
-- It surfaced while adding two unrelated columns to this table: the wider row
-- changed the width estimate, the planner picked differently, and the
-- byte-identical round-trip test read back "author" where it had always read
-- "title". Nothing about those columns broke the ordering — they revealed that
-- it had never been guaranteed, and that the test had been passing on luck.
-- Which is why this lands FIRST, on its own: the next migration should arrive on
-- a base where field order is a fact rather than a coincidence.
--
-- BACKFILL PRESERVES WHAT IS THERE NOW. ctid is the physical position, which is
-- exactly the accident the old ORDER BY was resolving; reading it once here and
-- freezing the result is the only backfill that does not reshuffle live forms.
-- ctid is unstable in general — that is precisely why this is a one-time read
-- into a durable column, and not a thing to query again.
--
-- created_at stays in the ORDER BY as the tiebreaker, so that a row inserted by
-- some path that has not learned about ordinal yet (it would land on the DEFAULT
-- of 0) sorts among its peers by time rather than by nothing at all.

ALTER TABLE content_type_fields
    ADD COLUMN IF NOT EXISTS ordinal INTEGER NOT NULL DEFAULT 0;

UPDATE content_type_fields f
SET ordinal = ranked.rn
FROM (
    SELECT id, (row_number() OVER (PARTITION BY content_type_id ORDER BY created_at, ctid))::INTEGER AS rn
    FROM content_type_fields
) AS ranked
WHERE f.id = ranked.id;
