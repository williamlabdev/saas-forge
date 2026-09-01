-- Entry revisions: what the content BECAME, version by version (ADR-014 §5).
--
-- §5 reopens half of ADR-006's exclusion — store, do not restore. The half it
-- does not reopen is load-bearing and this table must not be read as promising
-- it: restoring sends a whole old payload back through UpdateEntry, whose
-- guardWritableKeys REFUSES unwritable keys rather than skipping them
-- (service/content_service.go:872-881), so a restricted role could never
-- restore any entry holding a restricted field. Giving restore a "only the
-- keys you may write" semantics, or a bypass, are both new rulings — ADR-009
-- already refused the second. So what these rows buy is that the ruling stays
-- AVAILABLE: when the first real "put back what the agent broke" need appears,
-- the cost due is that semantic ruling, not the data, because the data is
-- already here.
--
-- THE DIVISION OF LABOUR WITH 000032. content_activity answers "who did what"
-- and deliberately holds no payload; this table answers "what the content
-- became" and holds nothing else. Neither is a degraded copy of the other, and
-- the field-masking consequence in 000032's header is exactly why they stay
-- apart: this table DOES hold every restricted field's value, which is why §6's
-- field-level masking becomes mandatory the day anything above the repository
-- reads it. Today nothing does — see the note on the read path below.

CREATE TABLE IF NOT EXISTS entry_revisions (
    -- (entry_id, version) is the key, and it is not a new invention: version is
    -- already `version + 1` on every applied write because it is the optimistic
    -- lock (postgres_repository.go's UpdateEntry), so the pair is unique for
    -- free. A separate revision sequence would be a second counter to keep in
    -- step with the first, with nothing to show for it.
    entry_id    UUID NOT NULL,
    version     INT  NOT NULL,

    tenant_id   TEXT NOT NULL,

    -- The working copy as it stood AT this version — the post-write value, not
    -- the pre-image. Reading "what was overwritten" is therefore reading the
    -- PREVIOUS row, which is the direction a version history is normally walked
    -- and the one that makes version 1 (a create, overwriting nothing) an
    -- ordinary row rather than a special case.
    --
    -- No size limit here beyond the one payload already has: handler.go:22 caps
    -- a payload at 1 MiB and quota's guardEntryBytes bounds the total, so this
    -- table's growth is bounded by the same two guards that bound `entries`.
    payload     JSONB NOT NULL,

    -- The actor trio, same vocabulary as content_activity and the entries
    -- provenance pairs. author_user_id is who ANSWERS for the write, not
    -- necessarily who typed: for an agent credential it is the minting
    -- principal.
    --
    -- NULLABLE, and for 000031's reason rather than 000032's. 000032 could make
    -- actor_kind NOT NULL because its recorder knows the kind first-hand. This
    -- recorder does not: it copies what the `entries` row holds after the write,
    -- and 000031 deliberately left that column nullable so "not recorded" stays
    -- expressible. Substituting 'human' while copying would put a specific false
    -- answer into the column whose whole purpose is to answer the question —
    -- 000031's own words for why it refused to backfill.
    author_kind     TEXT,
    author_user_id  UUID,
    author_agent_id TEXT,

    -- When the row reached this version. Copied from entries.updated_at rather
    -- than defaulted to now(), so a revision and the write that produced it
    -- carry the SAME instant: they are written in one transaction, and two
    -- clocks a few microseconds apart would make "the write at 10:03:07.1" and
    -- "the revision at 10:03:07.1" fail to join on the timestamp §1's
    -- attribution window is expressed in.
    created_at  TIMESTAMPTZ NOT NULL,

    PRIMARY KEY (entry_id, version),

    -- ON DELETE CASCADE: a hard delete takes the history with it.
    --
    -- This is a deliberate choice and not the lazy one, so its reasoning has to
    -- survive being read later. Keeping revisions past a delete would create a
    -- store of tenant content that no product path can ever remove — this table
    -- has no purge job and, by the append-only design below, no delete path at
    -- all — so "delete this entry" would stop meaning what it says. Against
    -- that: cascading loses exactly what §5 is buying, for the one operation
    -- that loses the most.
    --
    -- What settles it today is WHO CAN DELETE. agent_gate.go does not enumerate
    -- content:delete, and its comment gives the reason: deletion is blocked ON
    -- version history. So the threat §5 names — an unattended agent destroying
    -- content — cannot reach this path at all; a delete is a person's deliberate
    -- act on their own tenant's data.
    --
    -- ⚠️ THAT ARGUMENT EXPIRES THE DAY content:delete IS OPENED TO AGENTS, and
    -- this table existing is precisely the precondition ADR-013 §5 blocked that
    -- on. Opening it while this cascade stands would hand an agent a one-step
    -- permanent destruction of both the content and its history — ADR-014's own
    -- "the answer went out through the other door" shape, sprung by the very
    -- migration that was supposed to make the door safe. Whoever reopens
    -- content:delete must rule on retention here first.
    FOREIGN KEY (entry_id) REFERENCES entries (id) ON DELETE CASCADE,

    -- NULL passes: it is "not recorded", not a fourth kind (000031).
    CONSTRAINT entry_revisions_author_kind_check
        CHECK (author_kind IS NULL OR author_kind IN ('human', 'agent', 'service')),

    -- 000031's three-valued spelling, and it must be copied in that form rather
    -- than as the short biconditional: with a NULL kind beside an agent id the
    -- short form evaluates to NULL, and a CHECK accepts NULL, so the
    -- half-written state the constraint exists to refuse would walk straight in.
    CONSTRAINT entry_revisions_author_agent_kind_check
        CHECK (
            (author_kind IS NULL AND author_agent_id IS NULL)
            OR (
                author_kind IS NOT NULL
                AND ((author_kind = 'agent') = (author_agent_id IS NOT NULL))
            )
        ),

    -- An agent line names the principal who answers for it (ADR-013 §2).
    CONSTRAINT entry_revisions_author_agent_has_principal_check
        CHECK (author_kind IS DISTINCT FROM 'agent' OR author_user_id IS NOT NULL)
);

-- WHICH VERSIONS GET A ROW, AND WHICH ARE ABSENT ON PURPOSE.
--
-- Written by: CreateEntry (version 1) and UpdateEntry (every applied write).
-- Those are the only two statements that change `payload` for a single entry.
--
-- NOT written by SetEntryPublishState, which bumps `version` while leaving
-- `payload` untouched. Its rows would be byte-identical copies of the previous
-- revision, and what it actually changed — who released what, and when — is
-- content_activity's answer, not this table's. The consequence is that version
-- numbers here are SPARSE, so the read rule is "the content at version N is the
-- newest revision with version <= N", never "the row with version = N". A
-- published_version therefore resolves correctly without a row of its own: the
-- snapshot holds the working copy as it stood one version earlier, which is the
-- revision that lookup finds.
--
-- ALSO written by DeleteField and RenameField, the two bulk statements
-- (postgres_repository.go). They change `payload` and bump `version` across
-- every entry of a type, and this migration originally shipped without covering
-- them — the gap made the read rule above return a STALE payload (one still
-- holding the deleted key) for versions after a bulk mutation, a wrong answer
-- rather than a missing one, in the operation §5's purpose covers most.
--
-- Closing it needed a ruling this migration was not entitled to make, and
-- william made it on 2026-08-06: THREAD THE ACTOR THROUGH. Those statements
-- previously wrote no updated_by/_kind/_agent at all, so a revision copied off
-- the row would have attributed a schema admin's bulk deletion to whoever last
-- edited each entry — the specific false answer 000031 refused. Both methods now
-- take a domain.WriteActor and stamp it.
--
-- ⚠️ THE BULK WRITERS MUST NOT INSERT FOR EVERY ROW THEY TOUCH. Their WHERE
-- spans both copies, but `version` only moves when the WORKING copy held the
-- key — and this table's key is (entry_id, version). A revision for a row whose
-- version stood still re-inserts a version that already has a row, and there is
-- no ON CONFLICT here to swallow it (see recordEntryRevision for why that
-- absence is deliberate), so the whole bulk schema change fails. Both statements
-- guard the insert with a `working` CTE. That is the semantically right answer
-- as well as the safe one: a row whose working copy did not change has no new
-- version of its content to record. The path is reachable, not theoretical —
-- publish, then drop the key from the working copy, and the snapshot still
-- holds it.

-- The only query this table has: one entry's history, newest first. The PRIMARY
-- KEY already serves it (entry_id is its leading column), so no second index is
-- created — an unread index on the table that grows fastest is pure write cost.
--
-- THERE IS NO READ PATH ABOVE THE REPOSITORY, and that is scope, not an
-- oversight: §5 is "store", and exposing revisions means first satisfying §6's
-- field-level masking (驗證計畫第 10 條) against a table that holds every
-- restricted value in full. ListEntryRevisions exists for the tests that prove
-- the rows are written; no service or handler calls it.
--
-- RETENTION IS CAPPED AT 50 PER ENTRY ON THE READ, NOT HERE (william 0806).
-- The ruling was phrased as retention; it is landed as a LIMIT in
-- ListEntryRevisions rather than a purge job, because "no purge job and no
-- delete path at all" above is a PREMISE of the cascade argument, not a
-- description of it — a delete path would re-open that argument, and it is
-- entangled with the unruled question of whether content:delete opens to
-- agents. So this table is still unbounded in rows and bounded in what anyone
-- can see, and the storage half of the ruling is open on ADR-014's 未解項.
-- If a purge ever lands here, the cascade reasoning above has to be re-argued
-- in the same change rather than left standing on a sentence that stopped
-- being true.

-- Same two-layer isolation as every content table (000014): the app scopes by
-- tenant_id, RLS refuses what a forgotten WHERE would leak. FORCE so the owner
-- is subject too.
--
-- APPEND-ONLY BY OMISSION, as 000032: policies FOR SELECT and FOR INSERT, none
-- for UPDATE or DELETE, so those statements match no policy under a
-- non-superuser role. 000014's honesty note applies unchanged — a superuser
-- connection bypasses RLS entirely and the dev compose is one, so the primary
-- guarantee is that no code path rewrites these rows; this stops a future one
-- from doing it quietly.
--
-- The cascade above is not an exception to append-only. It fires from the
-- referenced row's deletion rather than from a DELETE aimed here, which is the
-- one removal this table's design admits, and its reasoning is on the
-- constraint itself.
ALTER TABLE entry_revisions ENABLE ROW LEVEL SECURITY;
ALTER TABLE entry_revisions FORCE ROW LEVEL SECURITY;
DROP POLICY IF EXISTS entry_revisions_tenant_read ON entry_revisions;
CREATE POLICY entry_revisions_tenant_read ON entry_revisions
    FOR SELECT USING (tenant_id = app_current_tenant());
DROP POLICY IF EXISTS entry_revisions_tenant_insert ON entry_revisions;
CREATE POLICY entry_revisions_tenant_insert ON entry_revisions
    FOR INSERT WITH CHECK (tenant_id = app_current_tenant());
