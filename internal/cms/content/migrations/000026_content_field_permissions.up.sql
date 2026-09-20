-- Field-level read/write permission, declared ON THE FIELD.
--
-- WHY IT CANNOT BE IN POSTGRES. An entry is one JSONB document, so there is no
-- column to grant on: RLS is row-level and column privileges do not reach inside
-- a document. Drupal enforces field permissions in the database because Drupal
-- gives every field its own table; that is the bill this repo declined when it
-- chose document storage. So the DECISION is in Go (service) and only the
-- DECLARATION lives here — which is the same split ADR-008 landed on when a
-- payload-format CHECK turned out to need a join it cannot have.
--
-- Two arrays rather than one role-rank column. A rank ("editor and above")
-- cannot express "only the legal reviewer touches the disclaimer", which is the
-- case that motivated this; and roles here are a fixed unordered set (D3), not a
-- ladder — owner outranks viewer in the capability matrix, not in the schema.
--
-- EMPTY MEANS UNRESTRICTED, and it is the default, so every pre-existing field
-- keeps its current behaviour with no backfill and no code path change. The
-- alternative — empty means nobody — would turn this migration into an outage.
-- The cost of this choice is that "restricted to nobody" is inexpressible; a
-- field nobody may write is a field you delete.
--
-- NON-EMPTY IS FAIL-CLOSED AGAINST THE DELIVERY EDGE. A public delivery
-- credential carries no tenant role, so it matches no non-empty set and is
-- refused the field. That is the only defensible reading: the alternative serves
-- a field an operator has just marked admin-only to the open internet.
--
-- MUTABLE, unlike `type` and `multiple` (ADR-007's one-way doors). Those are
-- immutable because flipping them invalidates STORED VALUES; a permission change
-- invalidates nothing — it changes who may see or send a value that stays
-- exactly as it is. Granting and revoking IS the feature, so a one-way door here
-- would mean the first mistake is permanent.
--
-- The CHECK is worth having and — unlike ADR-008's rejected payload-format
-- CHECK — is actually expressible: the legal role set is a constant, so no join
-- is needed. Kept in lockstep with the memberships CHECK (000012) and with
-- domain.AllowedFieldRoles(); a parity test reads this constraint the way
-- TestEntryStatusParity reads 000016's. A role that reaches this table around
-- the service is a row no policy decision can be made from.

ALTER TABLE content_type_fields
    ADD COLUMN IF NOT EXISTS read_roles  TEXT[] NOT NULL DEFAULT '{}',
    ADD COLUMN IF NOT EXISTS write_roles TEXT[] NOT NULL DEFAULT '{}';

ALTER TABLE content_type_fields
    DROP CONSTRAINT IF EXISTS content_type_fields_roles_check;
ALTER TABLE content_type_fields
    ADD CONSTRAINT content_type_fields_roles_check
    CHECK (
        read_roles  <@ ARRAY['owner', 'admin', 'editor', 'viewer']::TEXT[]
    AND write_roles <@ ARRAY['owner', 'admin', 'editor', 'viewer']::TEXT[]
    );
