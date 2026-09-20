-- Reverses 000030. Dropping the columns discards provenance: rows written by an
-- agent become indistinguishable from rows the principal typed themselves.
--
-- created_by is deliberately NOT touched. Its values stay correct under the
-- rollback — the principal really is who answers for those rows — and rewriting
-- them to NULL to "undo" the refinement would create exactly the unownable rows
-- ADR-013 §2 prevents, in the name of a rollback.

ALTER TABLE entries
    DROP CONSTRAINT IF EXISTS entries_created_by_agent_has_principal_check;
ALTER TABLE entries
    DROP CONSTRAINT IF EXISTS entries_created_by_agent_kind_check;
ALTER TABLE entries
    DROP CONSTRAINT IF EXISTS entries_created_by_kind_check;

ALTER TABLE entries
    DROP COLUMN IF EXISTS created_by_agent,
    DROP COLUMN IF EXISTS created_by_kind;
