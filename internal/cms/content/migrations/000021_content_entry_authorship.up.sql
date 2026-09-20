-- Authorship / audit columns on entries.
--
-- The schema could not answer "who published this". The service has always
-- known — every write goes through authorize(), which returns the authenticated
-- Subject — so this migration is about writing down what is already in hand,
-- not about new plumbing.
--
-- All three are NULLABLE and there is NO backfill. Existing rows genuinely have
-- no recorded author, and a sentinel ("system", a zero uuid) would be a
-- fabricated fact in an audit column, which is the thing that makes an audit
-- trail worthless. NULL means "not recorded" and every reader must render it as
-- unknown.
--
-- No FOREIGN KEY to users(id), despite auth_audit_events and tenant_memberships
-- having one. ON DELETE SET NULL would ERASE the audit answer at exactly the
-- moment it matters (the user is gone — who did this?), and CASCADE/RESTRICT are
-- worse. The content plane already declines cross-module FKs: entries.tenant_id
-- is a TEXT slug, not a reference to tenants.id. Stated cost: the id can dangle,
-- so any UI resolving it has to tolerate "unknown user".
--
-- No index. Nothing queries by author yet, and an index without a query is
-- speculation. ADR-006 records the trigger (an editor's "my entries" view).

ALTER TABLE entries
    ADD COLUMN IF NOT EXISTS created_by   UUID,
    ADD COLUMN IF NOT EXISTS updated_by   UUID,
    ADD COLUMN IF NOT EXISTS published_by UUID;

-- created_by / updated_by describe the ROW and the WORKING copy respectively.
-- published_by describes the SNAPSHOT, exactly as published_version does — so it
-- is written when the snapshot is taken and cleared when the snapshot is
-- retracted. That is ADR-006's standing rule ("every field describing Data must
-- describe the same copy") applied one level up, to metadata about the act
-- rather than the content: a retracted entry must not keep naming whoever last
-- released it, because there is nothing released to have a releaser.
--
-- Deliberately WEAKER than entries_published_snapshot_check's biconditional: a
-- published row may legitimately have NULL here. Every row predating this
-- migration does, and requiring a value would force exactly the fabricated one
-- the header above rejects.
ALTER TABLE entries
    DROP CONSTRAINT IF EXISTS entries_published_by_snapshot_check;
ALTER TABLE entries
    ADD CONSTRAINT entries_published_by_snapshot_check
    CHECK (status = 'published' OR published_by IS NULL);
