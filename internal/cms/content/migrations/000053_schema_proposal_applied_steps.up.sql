-- Partial approval (缺口計畫 5.1, ADR-013 §3 step 8 落地回饋與補裁 amendment
-- 2026-09-10): which of the stored plan's steps an approval actually ran.
--
-- WHY A NEW COLUMN AND NOT A DERIVATION. Before this column, "approved" meant
-- "every step ran" — canApply() refused the whole plan if anything in it was
-- refused or blocked, so there was nothing to record beyond the status. Partial
-- approval breaks that equivalence: two proposals can both read "approved" and
-- differ in what actually happened to the schema. Recomputing the answer on
-- read is not available, for 000038's own reason applied one level down — the
-- plan re-run at approval time is a point-in-time comparison, and by the time
-- somebody reads the row afterwards the live schema has moved on. What ran is a
-- fact about the past, not a projection of the present.
ALTER TABLE schema_proposals ADD COLUMN IF NOT EXISTS applied_steps JSONB;

-- NULL COVERS THREE CASES ON PURPOSE, and the read path must keep them apart
-- rather than collapsing them to the same rendering:
--   * pending / rejected / expired — nothing ever ran, so there is nothing to
--     record; this is the ordinary meaning of NULL here.
--   * approved before this migration — the row is real and something DID run
--     (every step, under the pre-5.1 all-or-nothing rule), but which indices
--     is not reconstructible from the row alone. Backfilling it as "every
--     index in the stored plan" would be a plausible-looking guess, not a
--     recorded fact, on 000038's reasoning about plan_proposer: this column
--     exists so a reader never has to trust a value nobody actually wrote.
--
-- Left NULL rather than backfilled for the second case; the service layer
-- renders both as "not recorded" (applied_steps omitted from the DTO), which
-- is the honest answer for a row this feature predates.
COMMENT ON COLUMN schema_proposals.applied_steps IS
    'JSON array of indices into the stored plan''s steps that this approval actually ran. NULL for pending/rejected/expired rows and for rows approved before 000053, where it is unrecorded rather than empty.';
