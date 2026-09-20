DROP TABLE IF EXISTS integration_outbox;
DROP TYPE IF EXISTS outbox_status;

ALTER TABLE users DROP COLUMN IF EXISTS status_version;

DROP TABLE IF EXISTS user_roles;
DROP TABLE IF EXISTS role_permissions;
DROP TABLE IF EXISTS permissions;
DROP TABLE IF EXISTS roles;
