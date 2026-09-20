ALTER TABLE refresh_tokens DROP COLUMN IF EXISTS tenant_id;
DROP TABLE IF EXISTS memberships;
DROP TABLE IF EXISTS tenants;
