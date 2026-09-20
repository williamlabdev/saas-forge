-- Provenance columns on entries: WHO ANSWERS FOR a write, and WHAT wrote it
-- (ADR-013 §2).
--
-- 000021 added created_by to answer "who". ADR-013 refines what that column
-- means rather than adding a third author column: created_by is now WHO IS
-- ANSWERABLE, and for an agent credential that is the principal who minted it,
-- not the agent. These two columns carry the part that refinement drops — that
-- the keystrokes were a bot's, and which bot.
--
-- WHY THIS IS NOT AN AUDIT-ONLY NICETY. own_only (000027) confines a role to
-- rows matching created_by, and a NULL author matches nothing — ADR-009 records
-- that as the one refusal indistinguishable from data loss. Had agent writes
-- left created_by NULL, every one of them would have become a permanently
-- unownable row, and enough of them would block a type from ever confining a
-- role again: a one-way ratchet designed in. Recording the principal is what
-- keeps that from existing, and entries_created_by_agent_has_principal_check
-- below is that decision written where it cannot be forgotten.
--
-- created_by_kind IS BACKFILLED, and that is not the fabricated-fact objection
-- 000021 raised against backfilling created_by. "Not recorded" was a genuine
-- unknown; "human" for a row written before any agent credential could be
-- minted is a derivable fact — nothing else could have written it, because the
-- delivery credential is refused every write at authorize().
--
-- No new index. own_only continues to bind created_by, so the partial index
-- from 000027 still serves it: an agent's own_only predicate binds the
-- principal, deliberately, so the index does not move (ADR-013 §2 amendment).

ALTER TABLE entries
    ADD COLUMN IF NOT EXISTS created_by_kind  TEXT NOT NULL DEFAULT 'human',
    ADD COLUMN IF NOT EXISTS created_by_agent TEXT;

-- The DEFAULT stays after the backfill, on purpose and with a stated cost.
-- Dropping it would make an insert path that forgets the column fail loudly,
-- which is the better failure — but the column is NOT NULL and several existing
-- test fixtures insert entries by hand, so keeping it is what lets those rows
-- carry the honest answer instead of the migration having to rewrite them. The
-- gap it leaves (a future path that forgets BOTH columns records a human) is
-- narrowed by the biconditional below: an agent id cannot be recorded without
-- the kind that goes with it.

ALTER TABLE entries
    DROP CONSTRAINT IF EXISTS entries_created_by_kind_check;
ALTER TABLE entries
    ADD CONSTRAINT entries_created_by_kind_check
    CHECK (created_by_kind IN ('human', 'agent', 'service'));

-- Biconditional, not one-way: created_by_agent names an agent, so it must be
-- present exactly when the kind says an agent wrote the row. A human row
-- carrying an agent id and an agent row carrying none are the same bug seen
-- from two sides.
ALTER TABLE entries
    DROP CONSTRAINT IF EXISTS entries_created_by_agent_kind_check;
ALTER TABLE entries
    ADD CONSTRAINT entries_created_by_agent_kind_check
    CHECK ((created_by_kind = 'agent') = (created_by_agent IS NOT NULL));

-- The ratchet guard. An agent-written row without an answerable principal is
-- the unownable row ADR-013 §2 exists to prevent, and the service is not the
-- only thing that will ever write this table.
ALTER TABLE entries
    DROP CONSTRAINT IF EXISTS entries_created_by_agent_has_principal_check;
ALTER TABLE entries
    ADD CONSTRAINT entries_created_by_agent_has_principal_check
    CHECK (created_by_kind <> 'agent' OR created_by IS NOT NULL);
