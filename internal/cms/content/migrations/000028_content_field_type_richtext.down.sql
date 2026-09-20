-- Reverses 000028: drops the widened _v2 constraint and RESTORES the 000024
-- constraint (without 'richtext').
--
-- Restoring is legal for a down migration exactly when the object being added
-- back is one this migration's OWN up dropped — the rollback guard verifies
-- that pairing, so the catalog after this down matches the catalog 000024's up
-- left behind, and 000024's own down still has its constraint to drop.
--
-- Refuses to run while any richtext field exists: re-adding the narrower CHECK
-- against surviving rows would fail anyway, but failing on the explicit guard
-- names the situation instead of surfacing a constraint violation.

DO $$
BEGIN
    IF EXISTS (SELECT 1 FROM content_type_fields WHERE field_type = 'richtext') THEN
        RAISE EXCEPTION 'cannot downgrade: richtext fields exist; delete them first';
    END IF;
END $$;

ALTER TABLE content_type_fields
    DROP CONSTRAINT IF EXISTS content_type_fields_field_type_check_v2;
ALTER TABLE content_type_fields
    ADD CONSTRAINT content_type_fields_field_type_check
    CHECK (field_type IN (
        'string', 'text', 'number', 'boolean', 'enum', 'date', 'datetime', 'file', 'relation'
    ));
