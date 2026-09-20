-- The eighth notification kind: 'entry_changes_requested' (ADR-014 Amendment:
-- review decisions, internal/cms/content/migrations/000051). A reviewer's
-- request-changes decision notifies the entry's last writer through
-- SystemNotifier.NotifyUser, exactly the same CHECK-constraint treatment
-- 000049's header explains for every other kind: the database is the single
-- point that can refuse a bad value no matter which code path wrote the row.
--
-- A SEPARATE MIGRATION FILE, deliberately not folded into 000051 even though
-- the two ship together: in_app_notifications belongs to this package's own
-- migration tree, and 000051 lives in internal/cms/content/migrations, which
-- is migrated on its own — with no notification-package migrations applied at
-- all — by that package's integration test suite (see
-- loadContentRLSMigrations in internal/cms/content/repository). An ALTER on
-- this table from over there would fail every one of those tests against a
-- database that has never heard of in_app_notifications, instead of adding
-- the one CHECK value it was trying to add.
ALTER TABLE in_app_notifications
    DROP CONSTRAINT IF EXISTS in_app_notifications_kind_check;
ALTER TABLE in_app_notifications
    ADD CONSTRAINT in_app_notifications_kind_check CHECK (kind IN (
        'general',
        'schema_proposal',
        'entry_pending_review',
        'schedule_succeeded',
        'schedule_failed',
        'schedule_stale',
        'webhook_dead',
        'entry_changes_requested'
    ));
