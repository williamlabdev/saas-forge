-- Draft / publish lifecycle for content entries (STRATEGY §三 Headless CMS gap 1;
-- ADR-004 makes this the prerequisite for the public delivery path — without a
-- status there is nothing for a public reader to filter on).
--
-- status is DISTINCT from version: version stays an optimistic lock (see
-- PostgresContentRepository.UpdateEntry), status is editorial state. Do not
-- conflate them; a version history is a separate, not-yet-scheduled increment.
--
-- Backfill choice: existing rows land on 'draft', NOT 'published'. Nothing is
-- publicly readable today (no public route exists yet), so defaulting to draft
-- is fail-closed — no pre-existing row can become world-readable the moment a
-- delivery path ships. Operators who want the old rows live must publish them
-- deliberately.

ALTER TABLE entries
    ADD COLUMN IF NOT EXISTS status       TEXT NOT NULL DEFAULT 'draft',
    ADD COLUMN IF NOT EXISTS published_at TIMESTAMPTZ;

-- Legal editorial states. Kept in lockstep with domain.AllowedStatuses(); a
-- parity test asserts the two do not drift.
ALTER TABLE entries
    DROP CONSTRAINT IF EXISTS entries_status_check;
ALTER TABLE entries
    ADD CONSTRAINT entries_status_check CHECK (status IN ('draft', 'published'));

-- published_at is set exactly when status = 'published'. Enforced in the DB so a
-- future writer (public delivery, an import job) cannot produce a row that says
-- published but carries no timestamp.
ALTER TABLE entries
    DROP CONSTRAINT IF EXISTS entries_published_at_check;
ALTER TABLE entries
    ADD CONSTRAINT entries_published_at_check
    CHECK ((status = 'published') = (published_at IS NOT NULL));

-- The public delivery read path: one tenant's published entries of one type,
-- newest first. Deliberately separate from idx_entries_tenant_type_created
-- (which serves the admin list across all statuses) — the public path is the
-- read-heavy one and should not scan drafts.
CREATE INDEX IF NOT EXISTS idx_entries_tenant_type_status_created
    ON entries (tenant_id, content_type_id, status, created_at DESC);
