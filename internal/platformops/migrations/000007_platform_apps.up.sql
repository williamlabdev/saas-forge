CREATE TABLE platform_apps (
    id UUID PRIMARY KEY,
    name TEXT NOT NULL,
    tenant_id TEXT NOT NULL,
    owner TEXT NOT NULL DEFAULT '',
    status TEXT NOT NULL DEFAULT 'active'
        CHECK (status IN ('active', 'paused', 'archived')),
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

CREATE INDEX idx_platform_apps_tenant_id ON platform_apps (tenant_id);
CREATE INDEX idx_platform_apps_status ON platform_apps (status);
CREATE INDEX idx_platform_apps_name_lower ON platform_apps (LOWER(name));

INSERT INTO platform_apps (id, name, tenant_id, owner, status, created_at, updated_at)
VALUES
    ('11111111-1111-1111-1111-111111111101', 'Acme Orders', 'tenant_acme', 'alex@acme.com', 'active', NOW(), NOW()),
    ('11111111-1111-1111-1111-111111111102', 'Beta Members', 'tenant_beta', 'ops@beta.io', 'paused', NOW(), NOW()),
    ('11111111-1111-1111-1111-111111111103', 'Legacy CRM', 'tenant_legacy', 'admin@legacy.corp', 'archived', NOW(), NOW());
