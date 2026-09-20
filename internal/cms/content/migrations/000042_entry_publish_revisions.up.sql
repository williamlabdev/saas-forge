-- Publish revisions: what went LIVE, release by release (ADR-018).
--
-- THIS IS NOT 000034's TABLE AND MUST NOT BE READ AS A RENAME OF IT.
-- `entry_revisions` (000034) records the WORKING COPY at every version that
-- changed it — create, update, and the two bulk field mutations — and 000034's
-- header says in as many words that a publish writes NO row there, because a
-- publish bumps `version` without touching `payload`. This table records the
-- opposite event: the release itself. One row per publish, holding the snapshot
-- that went out and the four facts about the release that `entry_revisions` has
-- nowhere to put — which release this was (revision_no), when it went live, who
-- answers for it, and whether a person pressed the button or a schedule fired.
--
-- The names are deliberately distinct rather than `content_entry_revisions`,
-- which would sit one underscore away from the table above and be mistaken for
-- it by every future reader in a hurry.
--
-- WHY THE PAYLOAD IS STORED AGAIN RATHER THAN RESOLVED OUT OF 000034. The bytes
-- are duplicated: the snapshot that went live is the working copy as it stood at
-- `published_version`, and 000034's sparse read rule ("the newest revision with
-- version <= N") already resolves it. Resolving was the first design and it was
-- dropped for one reason — 000034 has NO purge and its retention ruling is
-- landed as a read LIMIT with the storage half explicitly open on ADR-014's
-- 未解項. A restore path built on that table would have its durability decided
-- by a question nobody has answered yet, and the day someone answers it with a
-- purge, every publish revision older than the cut silently loses the payload it
-- promises. Copying 20 payloads per entry buys independence from that ruling.
-- ADR-018 §2 carries the full argument.

CREATE TABLE IF NOT EXISTS entry_publish_revisions (
    -- A surfaced id rather than the composite key alone. Unlike 000034, whose
    -- (entry_id, version) pair is free because `version` is already the
    -- optimistic lock, this table's ordinal is invented here and the API
    -- addresses a row by (entry, revision_no); a synthetic primary key keeps
    -- that addressing from also being the physical identity, so a later
    -- renumbering (there is none planned) would not have to rewrite references.
    id              UUID PRIMARY KEY,

    tenant_id       TEXT NOT NULL,
    entry_id        UUID NOT NULL,

    -- PER-ENTRY ORDINAL, STARTING AT 1, DENSE WHILE IT LASTS.
    --
    -- Not the entry version, and not shared with 000034's key. The number a
    -- person is shown has to be countable — "restore revision 3" — and
    -- `entries.version` is neither dense nor about releases: it moves on every
    -- draft save, so consecutive releases are version 4 and version 19.
    --
    -- Allocated as MAX+1 inside the publishing transaction. That is safe for the
    -- reason the optimistic lock is safe: the same transaction has just UPDATEd
    -- the entry row, so it holds that row's lock and no second publish of the
    -- same entry can be computing a number at the same moment. The UNIQUE
    -- constraint below is the backstop, and it is deliberately not paired with
    -- ON CONFLICT — recordPublishRevision's comment gives 000034's reasoning for
    -- refusing loudly.
    --
    -- RETENTION MAKES IT SPARSE AT THE BOTTOM, never at the top: the purge
    -- deletes the oldest, so numbers are contiguous within what survives and
    -- simply start above 1 once an entry has been published more than
    -- MaxPublishRevisionsPerEntry times. A gap in the middle is not reachable.
    revision_no     INT NOT NULL,

    -- The snapshot verbatim — `entries.published_payload` as this release left
    -- it. Bounded by the same two guards that bound `entries`: handler.go caps a
    -- payload at 1 MiB and quota's guardEntryBytes bounds the total, multiplied
    -- here by the retention cap rather than left to grow.
    payload         JSONB NOT NULL,

    -- The entry version this release put on the shelf — `published_version`
    -- after the publish, i.e. the value SetEntryPublishState writes. It is
    -- recorded because it is the join back to 000034's history and to a
    -- schedule's pinned_version, and because the console shows "v7 went live"
    -- rather than "release 3 went live" when an editor is reasoning about which
    -- draft this was.
    version         INT NOT NULL,

    -- WHEN THIS RELEASE HAPPENED, which is NOT entries.published_at.
    --
    -- That column means "first release since the last unpublish" and deliberately
    -- survives a re-publish unchanged (SetEntryPublishState's header). Copying it
    -- here would stamp every revision of a long-lived entry with the same
    -- instant, so the one question this table exists to answer — which of these
    -- was live in March — would have no answer at all. What is stored is the
    -- instant of THIS publish: the same `updated_at` the entry row just took, so
    -- the release and the row it produced share one clock reading, for the reason
    -- 000034 gives for copying updated_at rather than defaulting to now().
    published_at    TIMESTAMPTZ NOT NULL,

    -- Who answers for the release. Copied from entries.published_by after the
    -- write, so it is the value the CASE actually landed rather than what the
    -- caller passed. For a scheduled release that is the person who filed the
    -- schedule, never the worker (000041's requested_by).
    --
    -- NULLABLE, on 000031's principle rather than for a case anyone expects to
    -- hit: entries.published_by is nullable, and copying a specific false answer
    -- into the column whose whole purpose is to answer "who released this" is
    -- exactly what 000031 refused. A reader renders NULL as unknown.
    published_by    UUID,

    -- The mechanism, same closed vocabulary as content_activity.via (000041) and
    -- kept in lockstep with it by the identical CHECK. A publish revision and the
    -- activity line for the same release must not disagree about whether a person
    -- pressed the button.
    via             TEXT NOT NULL DEFAULT '',
    via_schedule_id UUID,

    created_at      TIMESTAMPTZ NOT NULL DEFAULT now(),

    -- ON DELETE CASCADE, and here the argument 000034 had to labour over is
    -- short. That table's cascade discards the only copy of overwritten working
    -- copies and its reasoning had to lean on who may delete; this one discards
    -- release history for a row that no longer exists, while content_activity —
    -- which does NOT cascade — keeps the record that the releases happened. What
    -- is lost is the payloads, and "delete this entry" is a request to lose
    -- exactly those.
    FOREIGN KEY (entry_id) REFERENCES entries (id) ON DELETE CASCADE,

    -- One row per (entry, ordinal). The backstop for the MAX+1 allocation above.
    CONSTRAINT uq_entry_publish_revisions_no UNIQUE (entry_id, revision_no),

    CONSTRAINT entry_publish_revisions_no_check CHECK (revision_no > 0),
    CONSTRAINT entry_publish_revisions_version_check CHECK (version > 0),

    CONSTRAINT entry_publish_revisions_via_check
        CHECK (via IN ('', 'schedule')),
    -- Biconditional, as 000041's: a 'schedule' row with no schedule to name is
    -- an audit hole, and a schedule id on a direct publish claims an approval
    -- that never happened.
    CONSTRAINT entry_publish_revisions_via_schedule_id_check
        CHECK ((via = 'schedule') = (via_schedule_id IS NOT NULL))
);

-- NO SECOND INDEX, deliberately, and this is 000034's ruling applied rather than
-- a new one: the only query is one entry's releases newest first, and the UNIQUE
-- constraint's index already leads with entry_id — Postgres reads it backwards
-- for ORDER BY revision_no DESC at no cost. An extra (entry_id, revision_no DESC)
-- index would be write cost on the table that grows with every release, bought
-- for nothing. ADR-018 lists it as a deliberate departure from the approved
-- design so it is not mistaken for an omission.

-- Same two-layer isolation as every content table (000014): the app scopes by
-- tenant_id, RLS refuses what a forgotten WHERE would leak, FORCE so the owner
-- is subject too.
--
-- A DELETE POLICY EXISTS HERE AND DOES NOT EXIST ON 000034, and the difference is
-- the point rather than an inconsistency. 000034 is append-only by omission
-- because its retention was landed as a read cap precisely to avoid opening a
-- delete path; this table's retention IS a purge (ADR-018 §1), so the statement
-- has to match a policy. There is still no UPDATE policy: a published revision is
-- a record of what went out, and nothing may edit it after the fact.
ALTER TABLE entry_publish_revisions ENABLE ROW LEVEL SECURITY;
ALTER TABLE entry_publish_revisions FORCE ROW LEVEL SECURITY;
DROP POLICY IF EXISTS entry_publish_revisions_tenant_read ON entry_publish_revisions;
CREATE POLICY entry_publish_revisions_tenant_read ON entry_publish_revisions
    FOR SELECT USING (tenant_id = app_current_tenant());
DROP POLICY IF EXISTS entry_publish_revisions_tenant_insert ON entry_publish_revisions;
CREATE POLICY entry_publish_revisions_tenant_insert ON entry_publish_revisions
    FOR INSERT WITH CHECK (tenant_id = app_current_tenant());
DROP POLICY IF EXISTS entry_publish_revisions_tenant_delete ON entry_publish_revisions;
CREATE POLICY entry_publish_revisions_tenant_delete ON entry_publish_revisions
    FOR DELETE USING (tenant_id = app_current_tenant());
