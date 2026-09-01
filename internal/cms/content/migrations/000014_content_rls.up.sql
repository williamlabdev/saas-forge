-- TKT-R6b: Postgres Row-Level Security as a SECOND isolation layer on the
-- content plane. The application already scopes every query by tenant_id
-- (primary defense); RLS is defense-in-depth — if a query ever forgets the
-- WHERE clause, the database still refuses cross-tenant rows.
--
-- Mechanism: policies key on the `app.tenant_id` GUC that the app sets per
-- transaction (see PostgresContentRepository.withTenant). When the GUC is
-- unset the comparison is against NULL → no rows match → fail-closed.
--
-- IMPORTANT — role model: a SUPERUSER connection bypasses RLS entirely, and a
-- table owner bypasses it unless FORCE is set. We FORCE it so the owner is
-- subject too, but PRODUCTION MUST CONNECT AS A NON-SUPERUSER ROLE for RLS to
-- take effect. The dev compose connects as the postgres superuser, so these
-- policies are present-but-dormant there (the app-layer WHERE still isolates);
-- they activate under a non-superuser production role. See readme.

-- current tenant from the per-transaction GUC. NULLIF maps BOTH unset (NULL)
-- AND empty-string ('') to NULL, so the policy comparison `tenant_id = NULL`
-- is NULL → no rows → fail-closed. This matters because a committed tx leaves
-- a residual '' on the pooled connection (SET LOCAL reset), and because
-- orphaned R1/R2 rows could carry tenant_id=''; both must see nothing, not
-- everything. Real tenant slugs are never empty, so nothing legitimate is hidden.
CREATE OR REPLACE FUNCTION app_current_tenant() RETURNS TEXT
    LANGUAGE sql STABLE AS $$ SELECT NULLIF(current_setting('app.tenant_id', true), '') $$;

ALTER TABLE content_types ENABLE ROW LEVEL SECURITY;
ALTER TABLE content_types FORCE ROW LEVEL SECURITY;
DROP POLICY IF EXISTS content_types_tenant_isolation ON content_types;
CREATE POLICY content_types_tenant_isolation ON content_types
    USING (tenant_id = app_current_tenant())
    WITH CHECK (tenant_id = app_current_tenant());

ALTER TABLE entries ENABLE ROW LEVEL SECURITY;
ALTER TABLE entries FORCE ROW LEVEL SECURITY;
DROP POLICY IF EXISTS entries_tenant_isolation ON entries;
CREATE POLICY entries_tenant_isolation ON entries
    USING (tenant_id = app_current_tenant())
    WITH CHECK (tenant_id = app_current_tenant());

-- content_type_fields carries no tenant_id (it hangs off content_types via
-- content_type_id) and holds schema metadata, not tenant documents. It is
-- intentionally left un-RLS'd; the app reaches it only through a content type
-- it already tenant-scoped. Promoting it to RLS (policy via a join to
-- content_types) is a future increment if field-definition leakage matters.
