-- Dropping the columns drops every permission declaration with them, which
-- OPENS access rather than closing it: every restricted field becomes readable
-- and writable by anyone the content verbs already admit. That is the honest
-- reverse of this migration and not something a down step can soften — there is
-- nowhere else to keep the declaration. Take an artifact export first if the
-- declarations matter; the artifact carries them.

ALTER TABLE content_type_fields
    DROP CONSTRAINT IF EXISTS content_type_fields_roles_check;
ALTER TABLE content_type_fields
    DROP COLUMN IF EXISTS write_roles,
    DROP COLUMN IF EXISTS read_roles;
