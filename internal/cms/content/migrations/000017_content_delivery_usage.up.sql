-- Per-tenant public delivery read volume (ADR-004 amendment: "負載歸屬").
--
-- Why a daily bucket and not a row per read: the delivery path is the
-- read-optimised, CDN-cacheable one. A write per read would put a DB round-trip
-- on every uncached public request AND serialise same-tenant traffic on one
-- row. The service therefore aggregates in process and flushes periodically, so
-- this table sees one UPSERT per tenant per flush interval, not per request.
--
-- Consequence to be honest about: counts are approximate at the edges — an
-- unflushed window is lost if the process dies. This is usage VISIBILITY, not
-- billing of record. Rate limiting (which actually protects the platform) lives
-- at the delivery edge and does not depend on this table.

CREATE TABLE IF NOT EXISTS content_delivery_usage (
    tenant_id  TEXT NOT NULL,
    day        DATE NOT NULL,
    reads      BIGINT NOT NULL DEFAULT 0,
    updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    PRIMARY KEY (tenant_id, day)
);

-- Same tenant isolation as the rest of the content plane (see 000014): policies
-- key on the per-transaction app.tenant_id GUC and fail closed when unset.
ALTER TABLE content_delivery_usage ENABLE ROW LEVEL SECURITY;
ALTER TABLE content_delivery_usage FORCE ROW LEVEL SECURITY;
DROP POLICY IF EXISTS content_delivery_usage_tenant_isolation ON content_delivery_usage;
CREATE POLICY content_delivery_usage_tenant_isolation ON content_delivery_usage
    USING (tenant_id = app_current_tenant())
    WITH CHECK (tenant_id = app_current_tenant());
