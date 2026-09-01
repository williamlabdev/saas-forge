DROP TRIGGER IF EXISTS trg_users_updated_at ON users;
DROP FUNCTION IF EXISTS set_users_updated_at();
DROP TABLE IF EXISTS users;
DROP TYPE IF EXISTS user_status;
