-- Widen the field_type constraint to admit 'richtext' (ADR-010).
--
-- Rich text stores structured block JSON — an array of typed blocks — under a
-- field type of its own rather than as a flag on `text`, because the two are
-- different VALUE SHAPES with different validators, and because `text` already
-- means "long string" to every consumer shipped; widening its meaning would
-- silently change what existing schemas accept.
--
-- This REPLACES the constraint 000024 introduced, and the replacement carries a
-- NEW NAME (_v2) on purpose: the rollback guard in rls_integration_test.go
-- tracks catalog objects by name, so a same-name drop-and-recreate is a change
-- it cannot see — its down would read as "ran and changed nothing". A versioned
-- name makes both directions visible: this up removes _check and adds _v2, the
-- down removes _v2 and restores _check. The parity test
-- (TestFieldTypeSQLParity) now reads THIS file as the field_type contract, in
-- lockstep with domain.AllowedFieldTypes().
--
-- The block grammar itself is validated in Go (domain.ValidateRichText); the
-- payload column stays JSONB with no per-shape CHECK, exactly as for every
-- other field type.

ALTER TABLE content_type_fields
    DROP CONSTRAINT IF EXISTS content_type_fields_field_type_check;
ALTER TABLE content_type_fields
    ADD CONSTRAINT content_type_fields_field_type_check_v2
    CHECK (field_type IN (
        'string', 'text', 'richtext', 'number', 'boolean', 'enum', 'date', 'datetime', 'file', 'relation'
    ));
