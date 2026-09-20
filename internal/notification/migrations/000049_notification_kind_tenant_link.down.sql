DROP INDEX IF EXISTS idx_notifications_user_tenant_created;

CREATE INDEX IF NOT EXISTS idx_notifications_user_created
    ON in_app_notifications (user_id, created_at DESC);

ALTER TABLE in_app_notifications
    DROP COLUMN IF EXISTS link,
    DROP COLUMN IF EXISTS kind,
    DROP COLUMN IF EXISTS tenant_id;
