-- Reverses 000029. Registered receivers are configuration, not content; a
-- rollback discards them and re-registration is the recovery path.

DROP POLICY IF EXISTS content_webhooks_tenant_isolation ON content_webhooks;
DROP INDEX IF EXISTS idx_content_webhooks_tenant_active;
DROP TABLE IF EXISTS content_webhooks;
