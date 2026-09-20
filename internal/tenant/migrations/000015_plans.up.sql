-- TKT-R4b PR1: plan tiers + per-tenant plan binding (metering + tiering).
-- This PR is data-layer
-- only — nothing reads these limits yet (content still uses the R4a static
-- Quota); PR2 switches enforcement to resolve limits from a tenant's plan.
--
-- Each dimension mirrors the R4a Quota fields; 0 = unlimited for that
-- dimension (same semantics as R4a, D5). soft_threshold_pct drives the
-- soft-warning signal added in PR2.
CREATE TABLE IF NOT EXISTS plans (
    name                TEXT PRIMARY KEY,
    max_types           INT NOT NULL DEFAULT 0,
    max_entries         INT NOT NULL DEFAULT 0,
    max_fields_per_type INT NOT NULL DEFAULT 0,
    max_entry_bytes     INT NOT NULL DEFAULT 0,
    soft_threshold_pct  INT NOT NULL DEFAULT 80 CHECK (soft_threshold_pct BETWEEN 1 AND 100),
    created_at          TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

-- Seed defaults — editable; the commercial values are the business's to tune
-- (plan names / tiers can change without touching the mechanism). 'pro'
-- mirrors the R4a defaults; 'enterprise' is all-zero = unlimited.
INSERT INTO plans (name, max_types, max_entries, max_fields_per_type, max_entry_bytes) VALUES
    ('free',        10,   1000,   50,  262144),
    ('pro',        100, 100000,  100, 1048576),
    ('enterprise',   0,      0,    0,       0)
ON CONFLICT (name) DO NOTHING;

-- Bind each tenant to a plan; default free. FK guarantees a tenant can only
-- point at a known plan. The 000012 demo tenants inherit 'free' automatically.
ALTER TABLE tenants ADD COLUMN IF NOT EXISTS plan TEXT NOT NULL DEFAULT 'free'
    REFERENCES plans (name);
