-- Reverses 000036. Dropping the table discards the record of which keys have
-- already been spent, so a retry that arrives afterwards creates a SECOND entry
-- rather than returning the first — the duplicate this table exists to prevent.
--
-- Nothing is salvaged on the way out and nothing can be: the entries themselves
-- are untouched (they are ordinary rows and were from the moment they were
-- created), and there is no column elsewhere that records which request made
-- one. Whoever runs this is choosing to be back where §9 started.

DROP TABLE IF EXISTS entry_idempotency;
