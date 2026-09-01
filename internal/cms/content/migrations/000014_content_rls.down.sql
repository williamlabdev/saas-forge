DROP POLICY IF EXISTS entries_tenant_isolation ON entries;
ALTER TABLE entries NO FORCE ROW LEVEL SECURITY;
ALTER TABLE entries DISABLE ROW LEVEL SECURITY;

DROP POLICY IF EXISTS content_types_tenant_isolation ON content_types;
ALTER TABLE content_types NO FORCE ROW LEVEL SECURITY;
ALTER TABLE content_types DISABLE ROW LEVEL SECURITY;

DROP FUNCTION IF EXISTS app_current_tenant();
