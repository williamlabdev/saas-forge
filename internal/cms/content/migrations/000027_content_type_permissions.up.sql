-- Data-level permission: which tenant roles may read and write the ENTRIES of a
-- content type, and which of them are confined to the entries they created.
--
-- This is the third and last layer of a stack whose parts answer different
-- questions, and conflating any two of them is how the whole thing stops being
-- reviewable:
--
--   verb   (authz)   — may this role write content in this tenant AT ALL
--   DATA   (here)    — which ENTRIES of this type may it touch
--   field  (000026)  — which KEYS of those entries' documents
--
-- The middle one was missing, and its absence was total: any editor could read,
-- rewrite, publish and delete every entry of every type their tenant had. A
-- salary table restricted key-by-key was still a collection anyone could list.
--
-- WHY IT IS DECLARED ON THE TYPE, like the field lists and unlike a policy file.
-- The declaration is then a fact about the schema, so it travels: into
-- GET /types, into the artifact, into git, into a review. A per-entry ACL would
-- express more and would be none of those things — an entry's permissions are
-- not a schema fact, so they cannot be diffed, and granting per USER (rather
-- than per role) needs a grant table, an admin surface to manage it, and an
-- answer to what happens when the user leaves. 000026 declined that trade for
-- fields for exactly these three reasons; declining it again here keeps one
-- shape rather than two.
--
-- WHY own_only_roles USES created_by RATHER THAN A NEW COLUMN. Authorship has
-- been recorded since 000021 and is already the answer to "whose is this". A
-- separate owner column would be a second, drifting answer to one question, and
-- the first write path that set one and not the other would decide the drift
-- silently.
--
-- EMPTY MEANS UNRESTRICTED, and it is the default — the same convention as
-- 000026, chosen the same way. Every type that exists today has empty lists, so
-- the opposite reading ("empty means nobody") would make this migration an
-- outage rather than a no-op. own_only_roles is empty by default too, which is
-- the same statement in its own terms: nobody is confined.
--
-- NON-EMPTY read_roles IS FAIL-CLOSED AGAINST THE DELIVERY EDGE, again matching
-- 000026: a public delivery credential carries no tenant role, so it matches no
-- non-empty set and the collection goes dark to the open internet. An operator
-- who restricts a type has restricted it; the alternative reading serves the
-- restricted type to everyone.
--
-- own_only_roles DOES NOT APPLY TO A DELIVERY CREDENTIAL, and that is a
-- different rule rather than an exception to the one above. Confinement is
-- "your rows within a collection you browse"; a public reader browses no
-- collection and authors nothing, and what it may see is decided by publish
-- state (ADR-004/006). Making it match on created_by would hide every published
-- entry from the public the moment a role was confined.
--
-- WHAT THIS CANNOT EXPRESS, on purpose and worth knowing before someone
-- rediscovers it as a bug:
--
--   * "read every entry, edit only your own" — one list confines BOTH
--     directions. The split is coherent and was declined for now to keep the
--     type carrying three lists rather than five; the trigger to revisit is a
--     real editorial workflow that needs cross-review of drafts.
--   * a drop box: write entries you may not read back. Refused for the reason
--     000026 refuses a write-only FIELD, and more strongly here — a PATCH is an
--     overlay on a stored document, so a writer who cannot read the document
--     cannot know what they are changing.
--
-- The CHECK pins membership to the same fixed role set as 000012 (memberships)
-- and 000026 (fields). A parity test reads it back; the set now has five homes
-- and the test is what stops the fifth from drifting.

ALTER TABLE content_types
    ADD COLUMN IF NOT EXISTS read_roles     TEXT[] NOT NULL DEFAULT '{}',
    ADD COLUMN IF NOT EXISTS write_roles    TEXT[] NOT NULL DEFAULT '{}',
    ADD COLUMN IF NOT EXISTS own_only_roles TEXT[] NOT NULL DEFAULT '{}';

ALTER TABLE content_types
    DROP CONSTRAINT IF EXISTS content_types_roles_check;
ALTER TABLE content_types
    ADD CONSTRAINT content_types_roles_check
    CHECK (
        read_roles     <@ ARRAY['owner', 'admin', 'editor', 'viewer']::TEXT[]
    AND write_roles    <@ ARRAY['owner', 'admin', 'editor', 'viewer']::TEXT[]
    AND own_only_roles <@ ARRAY['owner', 'admin', 'editor', 'viewer']::TEXT[]
    );

-- Confinement is a WHERE clause on every list, get, update, publish and delete
-- a confined role issues, so created_by stops being a column only an audit view
-- reads. The partial index carries the tenant and type scope the queries always
-- bind, and omits the rows the predicate can never match: created_by IS NULL is
-- unattributed authorship (rows predating 000021, and any non-human writer), and
-- confinement matches none of them.
CREATE INDEX IF NOT EXISTS idx_entries_author
    ON entries (tenant_id, content_type_id, created_by)
    WHERE created_by IS NOT NULL;
