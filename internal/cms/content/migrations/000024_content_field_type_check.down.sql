-- Reverses 000024, returning field_type to being constrained by Go alone.

ALTER TABLE content_type_fields
    DROP CONSTRAINT IF EXISTS content_type_fields_field_type_check;
