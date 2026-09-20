-- 2.5a: real in-app notifications need to say WHAT kind of thing happened
-- (kind), WHERE to send the reader (link), and WHICH tenant they belong to
-- (tenant_id) so a member of several tenants is not shown another tenant's
-- notice just because both tenants share their inbox.
--
-- tenant_id is NULLable, not backfilled, and stays that way going forward:
-- NULL means "not scoped to one tenant" (a future platform-wide notice, or
-- today's pre-2.5a rows and the existing self-notify POST /notifications,
-- which never named a tenant). GET /api/v1/notifications filters
-- `tenant_id = current tenant OR tenant_id IS NULL` for exactly this reason
-- — a NULL row is not "belongs to no tenant", it is "belongs to every
-- tenant view", so an old row a user wrote to themselves keeps showing up
-- rather than becoming unreachable the moment this migration runs. Every
-- trigger wired in 2.5a (schema proposal, entry pending review, schedule
-- outcome, webhook dead-letter) DOES set tenant_id — they are all tenant
-- events — so NULL is a semantic third state, not a stand-in for "unset by
-- an incomplete rollout".
--
-- kind is a fixed, small vocabulary, so it gets the same CHECK-constraint
-- treatment this repository already gives tenant_memberships.role and
-- platform_apps.status (migrations 000012, 000007): the database is the
-- single point that can refuse a bad value no matter which code path wrote
-- the row, including a future migration, a fixture, or a direct psql
-- session — not just this release's Go callers. The Go-side source of truth
-- (and the one place a caller has to look up the spelling) is
-- internal/notification/domain.Kind* / ValidKinds; this list must be kept
-- in lockstep with it, the same discipline authz.rego and rbac_authorizer.go
-- already carry for action names.
--
-- Application-layer validation is NOT the chosen enforcement because no
-- caller in this codebase ever lets kind arrive from outside internal Go
-- code: the one client-facing write, POST /notifications, does not accept a
-- kind field at all (it keeps the DEFAULT), and every other write goes
-- through SystemNotifier with one of the Kind* constants. A second
-- validation layer here would just be a second copy of the CHECK's list,
-- free to drift from it.
ALTER TABLE in_app_notifications
    ADD COLUMN tenant_id uuid NULL REFERENCES tenants (id) ON DELETE CASCADE,
    ADD COLUMN kind text NOT NULL DEFAULT 'general' CHECK (kind IN (
        'general',
        'schema_proposal',
        'entry_pending_review',
        'schedule_succeeded',
        'schedule_failed',
        'schedule_stale',
        'webhook_dead'
    )),
    ADD COLUMN link text NULL;

-- Superset of the original idx_notifications_user_created (000006): every
-- query this module runs already filters by user_id first, and the tenant
-- scoping + unread-count + list endpoints all also want created_at DESC, so
-- one composite index serves them instead of the old index plus a second.
-- The old index is dropped rather than left beside this one — a query
-- planner never benefits from two indexes that share the same leading
-- column and sort order, and an unused index still costs every write.
DROP INDEX IF EXISTS idx_notifications_user_created;

CREATE INDEX IF NOT EXISTS idx_notifications_user_tenant_created
    ON in_app_notifications (user_id, tenant_id, created_at DESC);
