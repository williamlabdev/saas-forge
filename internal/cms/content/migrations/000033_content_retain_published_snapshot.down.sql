-- Reverses 000033. This one DESTROYS DATA, and there is no way to write it so
-- it does not: the constraint being restored is the biconditional, which forbids
-- the rows this migration's up half exists to create. Every snapshot retained
-- across a retract has to go before the constraint can go back on, and the
-- snapshot is the only place that content was written down (entry revisions are
-- ADR-014 §5 and not built). Rolling back therefore loses exactly what §5.1 was
-- protecting: the last live version of every currently-retracted entry.
--
-- Order matters — clear the rows first, or the ADD CONSTRAINT fails on them.

UPDATE entries
   SET published_payload = NULL,
       published_version = NULL,
       published_by      = NULL
 WHERE status <> 'published'
   AND (published_payload IS NOT NULL OR published_version IS NOT NULL);

-- The retained asset references go with the snapshot that held them, or the
-- restored model would have entry_media_published rows for rows with no
-- snapshot — a state 000020's model cannot describe.
DELETE FROM entry_media_published
 WHERE entry_id IN (SELECT id FROM entries WHERE status <> 'published');

-- Drops the _v2 pair and puts 000020's and 000021's originals back — the one
-- legal way a down may add to the catalog, since those are exactly what this
-- migration's up dropped.
ALTER TABLE entries
    DROP CONSTRAINT IF EXISTS entries_published_snapshot_check_v2;
ALTER TABLE entries
    ADD CONSTRAINT entries_published_snapshot_check
    CHECK ((status = 'published') = (published_payload IS NOT NULL AND published_version IS NOT NULL));

ALTER TABLE entries
    DROP CONSTRAINT IF EXISTS entries_published_by_snapshot_check_v2;
ALTER TABLE entries
    ADD CONSTRAINT entries_published_by_snapshot_check
    CHECK (status = 'published' OR published_by IS NULL);

COMMENT ON TABLE entry_media_published IS NULL;
