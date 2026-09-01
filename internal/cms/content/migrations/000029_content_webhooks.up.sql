-- Tenant-registered webhooks for content-plane events (ADR-011).
--
-- One row per receiver. The SECRET is stored as written because HMAC signing
-- needs the plaintext at delivery time — this is a shared-secret scheme, not a
-- password, so hashing it would sign nothing. It is returned to the caller
-- exactly once (at registration); every later read of this table through the
-- API omits it. Encryption at rest is a listed trigger in ADR-011, not a
-- silent TODO.
--
-- No per-event subscription column in v1: the content event set is five types
-- with one audience (rebuild/purge). A receiver that cares about a subset
-- drops the rest on the floor; a `events text[]` filter is additive when a
-- real second audience appears.

CREATE TABLE IF NOT EXISTS content_webhooks (
    id          UUID PRIMARY KEY,
    tenant_id   TEXT NOT NULL,
    url         TEXT NOT NULL,
    secret      TEXT NOT NULL,
    active      BOOLEAN NOT NULL DEFAULT TRUE,
    description TEXT NOT NULL DEFAULT '',
    created_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
    -- The same shape the Go validator refuses; pinned here so a row written
    -- around the service cannot smuggle in a scheme the sender would follow.
    CONSTRAINT content_webhooks_url_check
        CHECK (char_length(url) BETWEEN 12 AND 2048 AND (url LIKE 'http://%' OR url LIKE 'https://%')),
    CONSTRAINT content_webhooks_secret_check CHECK (char_length(secret) BETWEEN 16 AND 128)
);

-- The delivery-time query is "active endpoints of ONE tenant"; the partial
-- index is that query's shape.
CREATE INDEX IF NOT EXISTS idx_content_webhooks_tenant_active
    ON content_webhooks (tenant_id) WHERE active;

-- Same two-layer isolation as every content table (000014): the app scopes by
-- tenant_id, RLS refuses what a forgotten WHERE would leak. FORCE so the table
-- owner is subject too.
ALTER TABLE content_webhooks ENABLE ROW LEVEL SECURITY;
ALTER TABLE content_webhooks FORCE ROW LEVEL SECURITY;
DROP POLICY IF EXISTS content_webhooks_tenant_isolation ON content_webhooks;
CREATE POLICY content_webhooks_tenant_isolation ON content_webhooks
    USING (tenant_id = app_current_tenant())
    WITH CHECK (tenant_id = app_current_tenant());
