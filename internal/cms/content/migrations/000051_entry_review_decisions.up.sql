-- Review decisions: a reviewer sends an entry back (ADR-014 Amendment:
-- review decisions).
--
-- WHAT THIS TABLE IS. ADR-014 §1 already has the approve half of review —
-- pressing publish IS the approval, and it needs no row of its own because the
-- entry's own published_version says "someone approved this version". What
-- was missing was the OTHER half: a reviewer who looked at a pending entry and
-- decided it is NOT ready, with a reason the author needs to see. That has no
-- state to piggyback on — an entry with unpublished changes looks identical
-- whether nobody has looked at it yet or a reviewer looked and rejected it
-- twice — so it gets its own append-only table, the same shape
-- content_entry_schedules (000041) and entry_publish_revisions (000042) both
-- took for "a fact about a moment in this entry's editorial life".
--
-- ONE VALUE IN `decision` TODAY, deliberately not an enum-free-text pair. The
-- amendment considered a full request/approve/changes-requested state machine
-- and rejected it: publish already IS approve, and a rejected-until-superseded
-- state would duplicate what entries.version vs this table's entry_version
-- already answers structurally (see the queue predicate this migration's
-- sibling change makes in postgres_repository.go). 'changes_requested' is
-- named as a value rather than the table being renamed
-- entry_changes_requests, so a second decision value has somewhere to go
-- later without a table rename.
--
-- entry_version IS THE LOAD-BEARING COLUMN, same role pinned_version plays in
-- content_entry_schedules: it is the version the decision was made AGAINST,
-- not a foreign key to a revision row. An entry whose CURRENT version still
-- equals a decision's entry_version is "sent back and not yet re-edited"; the
-- moment the author saves again, entries.version moves on and the decision
-- becomes historical without this table changing at all. No status column,
-- no "resolved" flag — the version comparison IS the resolution.
CREATE TABLE IF NOT EXISTS entry_review_decisions (
    id              UUID PRIMARY KEY,
    tenant_id       TEXT NOT NULL,

    entry_id        UUID NOT NULL,

    -- Denormalised from `entries`, same justification 000041 gives
    -- pinned_version's neighbour content_type_id: every read path here is
    -- keyed by entry alone (GetLatestEntryReviewDecision, the queue
    -- predicate, the history endpoint), so there is no query that wants this
    -- column, but authz.rego's per-type ReadRoles gate means a caller who can
    -- read the decision must already have resolved the entry's type via
    -- GetEntry first — nothing here needs it. Column omitted on purpose.

    -- 'changes_requested' is the only value today; see header.
    decision        TEXT NOT NULL,

    -- Never optional and never blank. A reviewer sending work back without
    -- saying why is the exact failure this feature exists to prevent — an
    -- author staring at "rejected" with no next step.
    reason          TEXT NOT NULL,

    -- THE VERSION THE REVIEWER WAS LOOKING AT. See header; this is what makes
    -- "sent back" a computed fact instead of a stored one.
    entry_version   INT NOT NULL,

    decided_by      UUID NOT NULL,
    decided_at      TIMESTAMPTZ NOT NULL DEFAULT now(),

    CONSTRAINT entry_review_decisions_decision_check
        CHECK (decision IN ('changes_requested')),

    CONSTRAINT entry_review_decisions_reason_check
        CHECK (btrim(reason) <> ''),

    CONSTRAINT entry_review_decisions_entry_version_check
        CHECK (entry_version > 0),

    -- Same cascade content_entry_schedules chose over content_activity's
    -- deliberate non-FK: a decision with no entry to attach to could only
    -- ever be dead weight, and the activity row recording it (entry.review.
    -- changes_requested) already carries the human-readable trace and does
    -- NOT cascade.
    FOREIGN KEY (entry_id) REFERENCES entries (id) ON DELETE CASCADE
);

-- The two reads this table serves: "what is the latest decision on this
-- entry" (queue predicate, EntryDTO decoration) and "full history, newest
-- first" (GET .../review-decisions). Both are answered by one index.
CREATE INDEX IF NOT EXISTS idx_entry_review_decisions_entry_recent
    ON entry_review_decisions (entry_id, decided_at DESC);

-- Append-only, same two-policy shape as content_activity (000032) and every
-- other history table in this schema: SELECT + INSERT only. A decision is
-- never edited or withdrawn — a reviewer who changes their mind approves the
-- next version, which is a NEW row (or a publish), not a mutation of this
-- one.
ALTER TABLE entry_review_decisions ENABLE ROW LEVEL SECURITY;
ALTER TABLE entry_review_decisions FORCE ROW LEVEL SECURITY;
DROP POLICY IF EXISTS entry_review_decisions_tenant_read ON entry_review_decisions;
CREATE POLICY entry_review_decisions_tenant_read ON entry_review_decisions
    FOR SELECT USING (tenant_id = app_current_tenant());
DROP POLICY IF EXISTS entry_review_decisions_tenant_insert ON entry_review_decisions;
CREATE POLICY entry_review_decisions_tenant_insert ON entry_review_decisions
    FOR INSERT WITH CHECK (tenant_id = app_current_tenant());

-- The notification kind CHECK (000049) needs an eighth value for this hook —
-- widened in internal/notification/migrations/000052, NOT here. in_app_
-- notifications belongs to the notification package's own migration tree, and
-- this package's integration tests migrate ONLY internal/cms/content/migrations
-- (see loadContentRLSMigrations in the repository package) against a bare
-- database that never runs the notification package's migrations at all — an
-- ALTER on that table here would 42P01 every one of those tests instead of the
-- one property it was trying to add. 000041's content_activity precedent does
-- not transfer: that crossing stayed inside this same migration tree, this one
-- would not.

