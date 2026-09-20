-- Provenance for the LAST WRITE, next to 000030's provenance for the FIRST one
-- (ADR-014 §4).
--
-- 000030 answered "what wrote this row into being". It cannot answer "what
-- touched it most recently", and that is the question the console's every-row
-- "who did this" column actually asks: an entry created by Alice in March and
-- edited by her agent at 10:03 today reads, under created_by_* alone, as
-- Alice's own work. updated_by has carried the WHO half since 000021; these two
-- carry the WHAT half that was missing beside it.
--
-- THESE COLUMNS ARE NULLABLE AND NOT BACKFILLED, and that is the one place this
-- migration deliberately departs from 000030.
--
-- 000030 backfilled created_by_kind = 'human' and defended it as a derivable
-- fact rather than a fabricated one: no agent credential could have written any
-- row that predated it, because the only other credential is refused every
-- write at authorize(). That argument does not survive being restated here.
-- Agent credentials exist now and they reach UpdateEntry today (the type
-- whitelist admits them — see agent_scope_test.go), so among the rows already
-- in this table there are ones whose last write was a bot's. Defaulting them to
-- 'human' would not be a conservative default; it would put a specific false
-- answer in the column whose entire purpose is to answer that question, and it
-- would do it silently and permanently.
--
-- NULL therefore means "not recorded", the third state ADR-014 §4 requires the
-- console to render as unknown — never falling back to a person's name. Every
-- write through the service records the trio from this migration forward, so
-- the unknown state is a shrinking population of historical rows, not an
-- ongoing gap.

ALTER TABLE entries
    ADD COLUMN IF NOT EXISTS updated_by_kind  TEXT,
    ADD COLUMN IF NOT EXISTS updated_by_agent TEXT;

-- NULL passes, on purpose: it is "not recorded", not a fourth kind.
ALTER TABLE entries
    DROP CONSTRAINT IF EXISTS entries_updated_by_kind_check;
ALTER TABLE entries
    ADD CONSTRAINT entries_updated_by_kind_check
    CHECK (updated_by_kind IS NULL OR updated_by_kind IN ('human', 'agent', 'service'));

-- The biconditional from 000030, restated for a nullable pair. It cannot be
-- copied verbatim: `(kind = 'agent') = (agent IS NOT NULL)` evaluates to NULL
-- when kind is NULL, and a CHECK passes on NULL — so the verbatim copy would
-- admit an agent id sitting next to an unrecorded kind, which is precisely the
-- half-written state the constraint exists to refuse.
--
-- Spelled as two admissible shapes rather than one clever expression:
-- both unrecorded, or a recorded kind that agrees with the agent id.
--
-- The `IS NOT NULL` in the second disjunct is not redundant with the first, and
-- omitting it reopened the hole this constraint is for — caught by the test, not
-- by reading. With a NULL kind and an agent id, the first disjunct is FALSE and
-- the second evaluates to NULL, and `FALSE OR NULL` is NULL, which a CHECK
-- accepts. Every branch has to be decidably true or false; a three-valued gap
-- anywhere in the expression is an open door.
ALTER TABLE entries
    DROP CONSTRAINT IF EXISTS entries_updated_by_agent_kind_check;
ALTER TABLE entries
    ADD CONSTRAINT entries_updated_by_agent_kind_check
    CHECK (
        (updated_by_kind IS NULL AND updated_by_agent IS NULL)
        OR (
            updated_by_kind IS NOT NULL
            AND ((updated_by_kind = 'agent') = (updated_by_agent IS NOT NULL))
        )
    );

-- The ratchet guard, mirroring 000030's. An agent write names a principal who
-- answers for it; a row claiming a bot did the last write with nobody
-- answerable is the shape ADR-013 §2 exists to keep out of this table.
ALTER TABLE entries
    DROP CONSTRAINT IF EXISTS entries_updated_by_agent_has_principal_check;
ALTER TABLE entries
    ADD CONSTRAINT entries_updated_by_agent_has_principal_check
    CHECK (updated_by_kind IS DISTINCT FROM 'agent' OR updated_by IS NOT NULL);
