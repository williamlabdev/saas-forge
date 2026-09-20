CREATE TABLE registration_idempotency (
    idempotency_key VARCHAR(128) PRIMARY KEY,
    user_id         UUID NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    created_at      TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE INDEX idx_registration_idempotency_created_at ON registration_idempotency (created_at);
