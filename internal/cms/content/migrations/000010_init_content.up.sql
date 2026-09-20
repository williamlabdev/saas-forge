-- Runtime-dynamic content model. Three generic tables serve any content type:
--   content_types        — a named, tenant-scoped schema
--   content_type_fields  — the columns of that schema (adding a row = adding a
--                          field at runtime, no deploy, no codegen)
--   entries              — documents, validated at write time, stored as JSONB
--
-- gen_random_uuid() is built into postgres 16 (pgcrypto not required).
-- tenant_id is TEXT to match authn.Subject.TenantID (set from X-Tenant-Id in dev).

CREATE TABLE IF NOT EXISTS content_types (
    id         UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id  TEXT NOT NULL,
    name       TEXT NOT NULL,
    label      TEXT NOT NULL DEFAULT '',
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    UNIQUE (tenant_id, name)
);

CREATE TABLE IF NOT EXISTS content_type_fields (
    id              UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    content_type_id UUID NOT NULL REFERENCES content_types(id) ON DELETE CASCADE,
    key             TEXT NOT NULL,
    field_type      TEXT NOT NULL,
    label           TEXT NOT NULL DEFAULT '',
    required        BOOLEAN NOT NULL DEFAULT FALSE,
    enum_values     TEXT[] NOT NULL DEFAULT '{}',
    relation_entity TEXT NOT NULL DEFAULT '',
    created_at      TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    UNIQUE (content_type_id, key)
);

CREATE TABLE IF NOT EXISTS entries (
    id              UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id       TEXT NOT NULL,
    content_type_id UUID NOT NULL REFERENCES content_types(id) ON DELETE CASCADE,
    payload         JSONB NOT NULL DEFAULT '{}'::jsonb,
    version         INT NOT NULL DEFAULT 1,
    created_at      TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at      TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

-- Primary access path: list a tenant's entries of one type, newest first.
CREATE INDEX IF NOT EXISTS idx_entries_tenant_type_created
    ON entries (tenant_id, content_type_id, created_at DESC);

-- Containment filters (payload @> '{"key":"value"}') are served by this GIN
-- index; jsonb_path_ops is the smaller/faster variant for @> queries.
CREATE INDEX IF NOT EXISTS idx_entries_payload_gin
    ON entries USING GIN (payload jsonb_path_ops);
