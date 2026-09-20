-- Dropping the column loses the proposer views recorded since 000038 and they
-- are not recomputable (see the up migration). That is acceptable for a
-- rollback: the approver's plan — the one an approval is decided against — is in
-- `plan` and is untouched here.
ALTER TABLE schema_proposals DROP COLUMN IF EXISTS plan_proposer;
