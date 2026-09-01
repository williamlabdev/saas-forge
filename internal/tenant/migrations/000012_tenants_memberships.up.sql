-- TKT-R1 PR1: tenant model (user <-> tenant many-to-many) + refresh token active tenant.
-- Numbering note: this landed as "000011_*" while it was being planned; global
-- numbering across modules makes it 000012.

CREATE TABLE IF NOT EXISTS tenants (
    id         UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    slug       TEXT NOT NULL UNIQUE,
    name       TEXT NOT NULL DEFAULT '',
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

CREATE TABLE IF NOT EXISTS memberships (
    user_id    UUID NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    tenant_id  UUID NOT NULL REFERENCES tenants (id) ON DELETE CASCADE,
    -- D3 fixed role set; issuance re-validates, this is the data layer's own guarantee
    role       TEXT NOT NULL CHECK (role IN ('owner', 'admin', 'editor', 'viewer')),
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    PRIMARY KEY (user_id, tenant_id)
);
-- No separate user_id index: the PK (user_id, tenant_id) leading column covers it.
CREATE INDEX IF NOT EXISTS idx_memberships_tenant ON memberships (tenant_id);

-- F2: refresh token remembers the active tenant it was issued for.
-- Nullable so in-flight old tokens degrade gracefully (plan section 6).
ALTER TABLE refresh_tokens ADD COLUMN IF NOT EXISTS tenant_id TEXT;

-- D11: seed the three demo tenants aligned with platform_apps seed (000007).
INSERT INTO tenants (id, slug, name) VALUES
    ('22222222-2222-2222-2222-222222222201', 'tenant_acme',   'Acme'),
    ('22222222-2222-2222-2222-222222222202', 'tenant_beta',   'Beta'),
    ('22222222-2222-2222-2222-222222222203', 'tenant_legacy', 'Legacy Corp')
ON CONFLICT (slug) DO NOTHING;
