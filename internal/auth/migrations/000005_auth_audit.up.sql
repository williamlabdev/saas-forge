CREATE TABLE auth_audit_events (
    id          UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    event_type  TEXT NOT NULL,
    outcome     TEXT NOT NULL,
    user_id     UUID REFERENCES users (id) ON DELETE SET NULL,
    client_ip   TEXT,
    user_agent  TEXT,
    error_code  TEXT,
    created_at  TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE INDEX idx_auth_audit_created_at ON auth_audit_events (created_at DESC);
CREATE INDEX idx_auth_audit_user_id ON auth_audit_events (user_id) WHERE user_id IS NOT NULL;
