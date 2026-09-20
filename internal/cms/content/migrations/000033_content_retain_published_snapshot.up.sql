-- Retract keeps the snapshot; only `status` moves (ADR-014 §5.1).
--
-- 000020 made the snapshot exist EXACTLY when the entry is published, and
-- SetEntryPublishState enforced the retract half by nulling the three snapshot
-- columns in the same CASE that writes them. ADR-014 §5.1 overturns that half.
--
-- WHY, and it is not a tidiness argument: ADR-014 §1 lets an agent write the
-- working copy and lets it unpublish, the second one unchecked because
-- "unpublish is damage control". That reason presumes unpublish is reversible.
-- It was not. An agent could overwrite `payload`, then unpublish, and the old
-- published content was gone — no human in the loop, nothing left to restore.
-- Two individually-permitted actions composed into exactly the destruction the
-- publish gate exists to prevent.
--
-- Keeping the snapshot does not by itself restore anything (re-publish still
-- snapshots the CURRENT working copy — entry revisions are ADR-014 §5, not this
-- migration). What it does is stop the record of what was live from being
-- destroyed by an unattended actor. That is the property the gate needs.

-- The biconditional loses one direction. What survives is the direction that
-- actually protects delivery: a published row MUST have something to serve.
-- The converse — "an unpublished row must have nothing" — is what §5.1 revokes.
--
-- Delivery does not lean on the revoked direction. Every public read path
-- filters on `status` explicitly and always has (GetEntry's IsPublished 404,
-- ListEntries' forced Status, ListTranslations' pair, and the media gate's
-- `e.status`), so relaxing this constraint does not widen what is served.
-- ADR-014 §5.1 predicted the opposite and flagged it as the ruling's biggest
-- risk; the grep it was inferred from returns nothing — there is no
-- `published_payload IS NOT NULL` predicate anywhere on a read path.
-- Renamed _v2 rather than swapped in place, for 000028's reason: the migration
-- rollback test tracks the catalog by NAME, so a same-name replacement is
-- invisible in both directions and reads as "ran and changed nothing".
ALTER TABLE entries
    DROP CONSTRAINT IF EXISTS entries_published_snapshot_check;
ALTER TABLE entries
    ADD CONSTRAINT entries_published_snapshot_check_v2
    CHECK (status <> 'published'
           OR (published_payload IS NOT NULL AND published_version IS NOT NULL));

-- published_by follows the snapshot, not the status (william 0806).
--
-- 000021 tied it to `status` and derived that from ADR-006's standing rule:
-- every field describing Data must describe the same copy. That derivation is
-- unchanged — it is the copy that moved. published_payload, published_version
-- and published_by are three faces of ONE snapshot, and 000021's own header
-- says published_by describes it "exactly as published_version does". Once the
-- snapshot survives a retract, clearing only published_by would leave the three
-- describing different copies, which is the very thing ADR-006 forbids.
--
-- The competing reading — clear it, because "it is not live" — was rejected for
-- a second reason: liveness is what `status` answers. §5.1 exists to move the
-- liveness test OFF snapshot-nullness and onto an explicit status predicate;
-- keeping published_by as a second liveness signal would plant the same
-- implicit condition again one column over.
--
-- The replacement is NOT "no constraint". The coupling is still enforced, just
-- against the snapshot rather than the status: you cannot name a releaser for a
-- snapshot that does not exist. Still deliberately one-directional, for
-- 000021's reason — rows predating that migration are published with NULL here,
-- and demanding a value would fabricate the actor 000021 refused to invent.
ALTER TABLE entries
    DROP CONSTRAINT IF EXISTS entries_published_by_snapshot_check;
ALTER TABLE entries
    ADD CONSTRAINT entries_published_by_snapshot_check_v2
    CHECK (published_by IS NULL OR published_payload IS NOT NULL);

-- entry_media_published outlives a retract too, and this is a correctness fix,
-- not a consequence.
--
-- 000020's header says this table is "written only at publish time, cleared on
-- unpublish", and its stated purpose is that the delivery gate must not read
-- the WORKING copy's references, because dropping an image from a draft would
-- revoke bytes the published snapshot still needs. A retained snapshot has
-- exactly that problem: its references would be gone while it still names the
-- assets, so nothing protects those bytes from a later revoke.
--
-- Retaining them does not widen delivery either — AssetIsPublished joins
-- entries and requires `e.status = 'published'`, so a retracted entry's rows sit
-- in this table without granting public access to anything.
COMMENT ON TABLE entry_media_published IS
    'Asset references held by the published SNAPSHOT. Written at publish; '
    'retained across a retract because the snapshot is (ADR-014 §5.1). '
    'Delivery joins entries.status — presence here is not public access.';
