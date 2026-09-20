-- Reverses 000052: narrows the kind CHECK back to the seven values 000049
-- established. Any row already written with kind = 'entry_changes_requested'
-- is left in place — narrowing a CHECK does not re-validate existing rows —
-- and would only be caught by a future INSERT/UPDATE that fails the narrower
-- constraint, the same latent-row behaviour every CHECK-narrowing down
-- migration in this schema accepts.
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
        'webhook_dead'
    ));
