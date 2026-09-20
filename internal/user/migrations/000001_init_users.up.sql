CREATE TYPE user_status AS ENUM ('active', 'suspended', 'deleted');

CREATE TABLE users (
    id                           UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    username                     VARCHAR(64) NOT NULL,
    username_lookup_hash         BYTEA NOT NULL,
    email_lookup_hash            BYTEA NOT NULL,
    email_encrypted              BYTEA NOT NULL,
    email_encrypted_nonce        BYTEA NOT NULL,
    display_name_encrypted       BYTEA,
    display_name_encrypted_nonce BYTEA,
    phone_encrypted              BYTEA,
    phone_encrypted_nonce        BYTEA,
    preferences                  JSONB NOT NULL DEFAULT '{}'::jsonb,
    status                       user_status NOT NULL DEFAULT 'active',
    created_at                   TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at                   TIMESTAMPTZ NOT NULL DEFAULT now(),
    deleted_at                   TIMESTAMPTZ,
    CONSTRAINT users_display_name_nonce_pair CHECK (
        (display_name_encrypted IS NULL) = (display_name_encrypted_nonce IS NULL)
    ),
    CONSTRAINT users_phone_nonce_pair CHECK (
        (phone_encrypted IS NULL) = (phone_encrypted_nonce IS NULL)
    ),
    CONSTRAINT users_deleted_consistency CHECK (
        (status = 'deleted' AND deleted_at IS NOT NULL)
        OR (status <> 'deleted' AND deleted_at IS NULL)
    )
);

CREATE UNIQUE INDEX uq_users_username_lookup_hash ON users (username_lookup_hash);
CREATE UNIQUE INDEX uq_users_email_lookup_hash ON users (email_lookup_hash);

CREATE INDEX idx_users_status_active
    ON users (status)
    WHERE status = 'active' AND deleted_at IS NULL;

CREATE INDEX idx_users_created_at ON users (created_at DESC);

CREATE INDEX idx_users_preferences_gin ON users USING GIN (preferences jsonb_path_ops);

CREATE OR REPLACE FUNCTION set_users_updated_at()
RETURNS TRIGGER AS $$
BEGIN
    NEW.updated_at = now();
    RETURN NEW;
END;
$$ LANGUAGE plpgsql;

CREATE TRIGGER trg_users_updated_at
    BEFORE UPDATE ON users
    FOR EACH ROW
    EXECUTE FUNCTION set_users_updated_at();
