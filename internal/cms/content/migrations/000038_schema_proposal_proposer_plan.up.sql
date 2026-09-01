-- The proposer's own view of the plan, stored beside the approver's
-- (ADR-013 未解項「提案者看不到自己提案的後續」).
--
-- WHY A SECOND COLUMN AND NOT A DERIVATION. 000037 stores the plan in the
-- APPROVER's scope, and that is load-bearing — re-running the plan at approval
-- time under a narrowed scope would make no agent proposal approvable. But the
-- proposer may not see that plan: it names the delete-type steps for every
-- content type outside the credential's whitelist, which is exactly what §4
-- keeps out of reach. So a proposer reading its own row needs the narrowed view,
-- and neither way of producing it on demand works:
--
--   * RECOMPUTING it on read consults today's live schema. The row would then
--     show a plan that quietly repairs a proposal that has gone stale — the
--     failure 000037's own comment on `plan` exists to prevent, arriving through
--     the other door. The proposer would see a clean plan and the approval would
--     still be refused with PROPOSAL_STALE.
--   * FILTERING the stored plan's steps after the fact breaks the counts.
--     Applicable/Refused/Blocked are computed from the same diff, so dropping
--     steps leaves the totals describing rows the caller cannot see —
--     visibleToAgent's comment says this in as many words, and it is the reason
--     the narrowing lives on the LIVE SIDE of the diff rather than on its output.
--
-- Both views are already computed when the proposal is filed (ProposeSchema
-- calls planScoped twice). This column stores the one that was being thrown
-- away.
ALTER TABLE schema_proposals ADD COLUMN IF NOT EXISTS plan_proposer JSONB;

-- BACKFILL IS EXACT FOR EVERY NON-AGENT ROW, not an approximation: visibleToAgent
-- returns the live types untouched when the subject is not an agent
-- (`if !sub.IsAgent() { return cts }`), so for a human or service proposer the
-- two views are the same document. Copying is therefore the true value, not a
-- stand-in for one.
UPDATE schema_proposals SET plan_proposer = plan WHERE proposed_by_kind <> 'agent';

-- AGENT ROWS FILED BEFORE THIS MIGRATION STAY NULL, and the column stays
-- nullable to say so. Their proposer view cannot be reconstructed: it is a
-- function of the tenant's live schema AT THE MOMENT THE ROW WAS FILED, and
-- nothing stores that. Writing today's narrowing into an old row would be the
-- recompute this column exists to avoid, with the added lie of looking stored.
--
-- NULL therefore means "not recorded", and the read path renders it as the
-- absence of a plan rather than as an empty one — an empty PlanResult reads as
-- "this proposal would change nothing", which is a different and false claim.
COMMENT ON COLUMN schema_proposals.plan_proposer IS
    'PlanResult in the PROPOSER''s scope. NULL only on agent rows filed before 000038, where it is unreconstructible; never an empty plan.';
