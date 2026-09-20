-- Reversing 000027. Dropping the columns drops the declarations with them: a
-- tenant that had restricted a collection comes back UNRESTRICTED, because empty
-- means unrestricted (see the up migration). That is the honest direction for a
-- down migration to fail in only because the code that enforces it is going away
-- in the same rollback — a schema that still carried the lists while nothing read
-- them would be the dangerous half.

DROP INDEX IF EXISTS idx_entries_author;

ALTER TABLE content_types
    DROP CONSTRAINT IF EXISTS content_types_roles_check;

ALTER TABLE content_types
    DROP COLUMN IF EXISTS own_only_roles,
    DROP COLUMN IF EXISTS write_roles,
    DROP COLUMN IF EXISTS read_roles;
