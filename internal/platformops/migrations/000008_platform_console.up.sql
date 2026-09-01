CREATE TABLE platform_billing_config (
    id SMALLINT PRIMARY KEY DEFAULT 1 CHECK (id = 1),
    plan_name TEXT NOT NULL,
    renews_at DATE NOT NULL,
    payment_status TEXT NOT NULL DEFAULT 'current',
    apps_quota INT NOT NULL DEFAULT 50,
    seats_quota INT NOT NULL DEFAULT 15,
    updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

CREATE TABLE platform_invoices (
    id TEXT PRIMARY KEY,
    issued_at DATE NOT NULL,
    amount TEXT NOT NULL,
    status TEXT NOT NULL
);

CREATE TABLE platform_staff (
    id UUID PRIMARY KEY,
    name TEXT NOT NULL,
    email TEXT NOT NULL UNIQUE,
    role TEXT NOT NULL,
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

CREATE TABLE platform_alerts (
    id UUID PRIMARY KEY,
    title TEXT NOT NULL,
    alert_type TEXT NOT NULL,
    read_at TIMESTAMPTZ,
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

INSERT INTO platform_billing_config (id, plan_name, renews_at, payment_status, apps_quota, seats_quota)
VALUES (1, 'Pro plan', '2026-06-15', 'current', 50, 15);

INSERT INTO platform_invoices (id, issued_at, amount, status) VALUES
    ('inv_01', '2026-05-01', '$299', 'Paid'),
    ('inv_02', '2026-04-01', '$299', 'Paid');

INSERT INTO platform_staff (id, name, email, role) VALUES
    ('22222222-2222-2222-2222-222222222201', '陳雅婷', 'yating@platform.internal', 'platform_admin'),
    ('22222222-2222-2222-2222-222222222202', '王志明', 'zhiming@platform.internal', 'support');

INSERT INTO platform_alerts (id, title, alert_type, read_at, created_at) VALUES
    ('33333333-3333-3333-3333-333333333301', 'Scheduled maintenance · 2026-06-01', 'maintenance', NULL, '2026-05-20'),
    ('33333333-3333-3333-3333-333333333302', 'Invoice payment failed · tenant_beta', 'billing', NOW(), '2026-05-18');
