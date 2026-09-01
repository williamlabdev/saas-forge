-- TKT-R1 PR-invite (plan D4 second half): invite a user into an existing
-- tenant. An invite is a pending membership: single-use token (hash at rest),
-- bound to the invitee's email (blind-index hash, no plaintext PII), with a
-- tenant-scoped role from the D3 set minus owner (ownership only via the
-- future transfer flow).
CREATE TABLE IF NOT EXISTS tenant_invites (
    id                UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id         UUID NOT NULL REFERENCES tenants (id) ON DELETE CASCADE,
    -- Blind index of the invitee email (same HMAC pepper as users.email_lookup_hash).
    email_lookup_hash BYTEA NOT NULL,
    role              TEXT NOT NULL CHECK (role IN ('admin', 'editor', 'viewer')),
    -- sha256 hex of the raw invite token; the raw token is returned once at
    -- creation and never stored (same pattern as refresh_tokens.token_hash).
    token_hash        TEXT NOT NULL UNIQUE,
    invited_by        UUID NOT NULL REFERENCES users (id),
    created_at        TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    expires_at        TIMESTAMPTZ NOT NULL,
    accepted_at       TIMESTAMPTZ,
    accepted_by       UUID REFERENCES users (id)
);

CREATE INDEX IF NOT EXISTS idx_tenant_invites_tenant ON tenant_invites (tenant_id);
