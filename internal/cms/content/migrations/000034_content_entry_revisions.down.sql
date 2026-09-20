-- Reverses 000034. Dropping the table discards every stored version, and those
-- values exist nowhere else: `entries` holds the current working copy and one
-- published snapshot, so everything a revision preserves is what those two have
-- already moved past.
--
-- Nothing is salvaged on the way out. There is no column on another table to
-- leave behind (unlike 000031's down migration) and no partial rollback that
-- would keep the history while removing the mechanism — the history IS the
-- table. Whoever runs this is choosing to be back where §5 started, which is
-- the state where an overwritten value is simply gone.

DROP TABLE IF EXISTS entry_revisions;
