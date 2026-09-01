-- Reverses 000032. Dropping the table discards the record of who did what —
-- including every refused action, which exists nowhere else: a 403 that was
-- answered to the caller and filed here leaves no other trace.
--
-- There is nothing to preserve on the way out, unlike 000031's down migration:
-- this table adds no column to anything and denormalises only a label. Whoever
-- runs this is choosing to lose the history, and no partial rollback would make
-- that choice smaller.

DROP INDEX IF EXISTS idx_content_activity_entry_time;
DROP INDEX IF EXISTS idx_content_activity_tenant_time;
DROP TABLE IF EXISTS content_activity;
