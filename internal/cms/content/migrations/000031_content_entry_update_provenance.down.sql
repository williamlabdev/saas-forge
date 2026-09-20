-- Reverses 000031. Dropping the columns discards last-write provenance: an
-- entry whose most recent edit was an agent's becomes indistinguishable from
-- one its owner edited by hand.
--
-- updated_by is deliberately NOT touched, for the same reason 000030's down
-- migration leaves created_by alone. Its values stay true under the rollback —
-- the principal really is who answers for the last write — and nulling them to
-- "undo" the pairing would destroy the WHO half that predates this migration
-- entirely (000021).

ALTER TABLE entries
    DROP CONSTRAINT IF EXISTS entries_updated_by_agent_has_principal_check;
ALTER TABLE entries
    DROP CONSTRAINT IF EXISTS entries_updated_by_agent_kind_check;
ALTER TABLE entries
    DROP CONSTRAINT IF EXISTS entries_updated_by_kind_check;

ALTER TABLE entries
    DROP COLUMN IF EXISTS updated_by_agent,
    DROP COLUMN IF EXISTS updated_by_kind;
