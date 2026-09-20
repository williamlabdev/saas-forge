-- Constrain content_type_fields.field_type in the DATABASE.
--
-- It has never been constrained here: domain.ValidFieldType at definition time
-- was the only gate, unlike `status` (000016) and `locale` (000018), which are
-- checked both in Go and in SQL. That was survivable while buildField was the
-- single door. Schema mutation multiplies the ways a field definition gets
-- written, and validateScalar's fail-closed default arm (added alongside
-- multi-valued fields) turns a bogus type into a hard refusal at write time
-- rather than a silently unvalidated value — so the remaining gap is a row that
-- got here around the service at all. Pin the set where it cannot be bypassed.
--
-- Kept in lockstep with domain.AllowedFieldTypes(); the parity test reads this
-- constraint the way TestEntryStatusParity reads 000016's. If this migration
-- fails to apply, some row holds a type the application does not recognise —
-- that is the finding, not an obstacle to route around.

ALTER TABLE content_type_fields
    DROP CONSTRAINT IF EXISTS content_type_fields_field_type_check;
ALTER TABLE content_type_fields
    ADD CONSTRAINT content_type_fields_field_type_check
    CHECK (field_type IN (
        'string', 'text', 'number', 'boolean', 'enum', 'date', 'datetime', 'file', 'relation'
    ));
