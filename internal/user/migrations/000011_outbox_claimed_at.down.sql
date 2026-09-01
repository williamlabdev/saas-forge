DROP INDEX IF EXISTS idx_integration_outbox_processing;
ALTER TABLE integration_outbox DROP COLUMN IF EXISTS claimed_at;
