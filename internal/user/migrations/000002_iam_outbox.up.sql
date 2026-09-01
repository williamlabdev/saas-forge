-- IAM facts (normalized RBAC; policy lives in OPA Rego)
CREATE TABLE roles (
    id          UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    name        VARCHAR(64) NOT NULL UNIQUE,
    description TEXT,
    created_at  TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE TABLE permissions (
    id          UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    resource    VARCHAR(64) NOT NULL,
    action      VARCHAR(64) NOT NULL,
    created_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
    CONSTRAINT uq_permissions_resource_action UNIQUE (resource, action)
);

CREATE TABLE role_permissions (
    role_id       UUID NOT NULL REFERENCES roles (id) ON DELETE CASCADE,
    permission_id UUID NOT NULL REFERENCES permissions (id) ON DELETE CASCADE,
    PRIMARY KEY (role_id, permission_id)
);

CREATE TABLE user_roles (
    user_id    UUID NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    role_id    UUID NOT NULL REFERENCES roles (id) ON DELETE CASCADE,
    expires_at TIMESTAMPTZ,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (user_id, role_id)
);

CREATE INDEX idx_user_roles_user_id ON user_roles (user_id);

-- MCP sync: transactional outbox
CREATE TYPE outbox_status AS ENUM ('pending', 'processing', 'done', 'failed');

ALTER TABLE users
    ADD COLUMN IF NOT EXISTS status_version INT NOT NULL DEFAULT 1;

CREATE TABLE integration_outbox (
    id            UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    aggregate_id  UUID NOT NULL,
    event_type    VARCHAR(64) NOT NULL,
    payload       JSONB NOT NULL DEFAULT '{}'::jsonb,
    status        outbox_status NOT NULL DEFAULT 'pending',
    retry_count   INT NOT NULL DEFAULT 0,
    idempotency_key VARCHAR(128) NOT NULL,
    last_error    TEXT,
    created_at    TIMESTAMPTZ NOT NULL DEFAULT now(),
    processed_at  TIMESTAMPTZ
);

CREATE UNIQUE INDEX uq_integration_outbox_idempotency ON integration_outbox (idempotency_key);
CREATE INDEX idx_integration_outbox_pending ON integration_outbox (created_at)
    WHERE status = 'pending';

-- Seed canonical roles for dev / OPA facts
INSERT INTO roles (name, description) VALUES
    ('admin', 'Full administrative access'),
    ('member', 'Standard end-user')
ON CONFLICT (name) DO NOTHING;
