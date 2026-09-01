-- Schema proposals: a schema change filed for a PERSON to approve
-- (ADR-013 §3 step 8).
--
-- WHAT THIS TABLE IS NOT. It is not the thing that stops an agent from
-- reshaping the schema — content:schema:write is, and no agent holds it. The
-- ADR's first draft got this backwards and the correction is worth keeping in
-- front of whoever reads this table next: a proposal endpoint cannot refuse a
-- caller who never uses it. What the table buys is the approval flow and the
-- audit trail, which the verb alone cannot express: WHO asked, for WHAT, WHEN,
-- and who answered.
--
-- THE PLAN IS STORED AS THE APPROVER WOULD SEE IT, not as the proposer saw it
-- (william ruled 2026-08-06). That distinction is not pedantic — it is what
-- makes the feature work at all. PlanSchema narrows the LIVE side of the diff
-- to an agent's whitelist (visibleToAgent, 補裁 E), so an agent's plan omits
-- the delete-type steps for every type it may not see. Storing that view and
-- then re-running the plan at approval time under the approver's full scope
-- would compare two different questions, and the counts would differ in every
-- tenant that has a second content type: no agent proposal would ever be
-- approvable. So the row carries the full-scope plan — what applying this
-- document would ACTUALLY do — while the proposer's HTTP response still shows
-- only its own narrowed view, which is what keeps §4's reach guarantee intact.
-- This is also why an agent cannot read the queue: the stored plan names types
-- outside its whitelist.

CREATE TABLE IF NOT EXISTS schema_proposals (
    id         UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id  TEXT NOT NULL,

    -- The document itself, verbatim. Stored rather than referenced because a
    -- proposal is a request to apply THIS text: re-deriving it from anywhere
    -- else at approval time would let the thing approved differ from the thing
    -- filed, which is the whole failure the flow exists to prevent.
    artifact   JSONB NOT NULL,

    -- Part of the request, not of the document (ADR-008): the same artifact is
    -- an overlay with prune off and an authority with it on. A proposal that
    -- did not record it would be approvable into either meaning.
    prune      BOOLEAN NOT NULL,

    -- The PlanResult computed when the proposal was filed, in the approver's
    -- scope (see above). Two jobs, and both need it stored rather than
    -- recomputed on read: it is what the approver reviews, and it is the
    -- baseline the re-run is compared against at approval time.
    plan       JSONB NOT NULL,

    -- 'expired' IS DELIBERATELY NOT A VALUE HERE. Expiry is a fact about the
    -- clock, and a stored status would need someone to run a sweeper for the
    -- row to become true — until then the column would say `pending` about a
    -- proposal nobody may approve. The API derives it (status = 'pending' AND
    -- expires_at <= now()), so there is no window in which the two disagree and
    -- no job to forget to schedule.
    status     TEXT NOT NULL DEFAULT 'pending'
        CHECK (status IN ('pending', 'approved', 'rejected')),

    -- WHO ASKED, in the same three columns as entry provenance (000030), and
    -- for the same reason: the person answerable for an agent is the agent's
    -- principal, so a proposal filed by a bot still names a human. No FK to
    -- users — this is a history record and must survive the account being
    -- deleted (the 000035 revoked_by argument, one table over).
    proposed_by       UUID NOT NULL,
    proposed_by_kind  TEXT NOT NULL
        CHECK (proposed_by_kind IN ('human', 'agent', 'service')),
    proposed_by_agent TEXT,

    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),

    -- Written by the application, not defaulted here (the 000035 expires_at
    -- precedent). The TTL is one number in one place in Go; a DEFAULT would be
    -- a second copy of it that only fires for rows the service did not write.
    expires_at TIMESTAMPTZ NOT NULL,

    -- Who answered, and when. Null while pending.
    decided_at TIMESTAMPTZ,
    decided_by UUID,

    -- Biconditional, and written as an equality between two IS NULL tests
    -- because `decided_at IS NULL OR decided_by IS NOT NULL` evaluates to NULL
    -- on exactly the row that matters, and a CHECK that evaluates to NULL
    -- PASSES (the trap 000030 walked into).
    CONSTRAINT schema_proposals_decision_complete
        CHECK ((decided_at IS NULL) = (decided_by IS NULL)),

    -- A decision and a status that disagree is the state that would make the
    -- audit trail lie: `approved` with no decider, or `pending` with one.
    CONSTRAINT schema_proposals_decided_iff_not_pending
        CHECK ((status = 'pending') = (decided_at IS NULL)),

    -- An agent name means the kind is agent, and vice versa — the same
    -- biconditional as entries_created_by_agent_kind_check (000030).
    CONSTRAINT schema_proposals_agent_kind
        CHECK ((proposed_by_kind = 'agent') = (proposed_by_agent IS NOT NULL))
);

-- The queue: pending proposals for one tenant, oldest first is how a queue is
-- worked, but the list is rendered newest-first like every other admin list, so
-- the index carries created_at DESC and the ordering choice stays in the query.
CREATE INDEX IF NOT EXISTS idx_schema_proposals_tenant
    ON schema_proposals (tenant_id, created_at DESC);

-- Same two-layer isolation as every content table (000014): the app scopes by
-- tenant_id, RLS refuses what a forgotten WHERE would leak. FORCE so the owner
-- is subject too.
--
-- UPDATE is granted here, unlike 000036: a proposal is decided in place, which
-- is the one thing that ever rewrites one of these rows. DELETE is not — an
-- expired proposal stays as a record of what was asked and never answered,
-- which is the ruling (2026-08-06: TTL turns it unusable, it does not remove
-- it).
ALTER TABLE schema_proposals ENABLE ROW LEVEL SECURITY;
ALTER TABLE schema_proposals FORCE ROW LEVEL SECURITY;
DROP POLICY IF EXISTS schema_proposals_tenant_read ON schema_proposals;
CREATE POLICY schema_proposals_tenant_read ON schema_proposals
    FOR SELECT USING (tenant_id = app_current_tenant());
DROP POLICY IF EXISTS schema_proposals_tenant_insert ON schema_proposals;
CREATE POLICY schema_proposals_tenant_insert ON schema_proposals
    FOR INSERT WITH CHECK (tenant_id = app_current_tenant());
DROP POLICY IF EXISTS schema_proposals_tenant_update ON schema_proposals;
CREATE POLICY schema_proposals_tenant_update ON schema_proposals
    FOR UPDATE USING (tenant_id = app_current_tenant())
    WITH CHECK (tenant_id = app_current_tenant());
