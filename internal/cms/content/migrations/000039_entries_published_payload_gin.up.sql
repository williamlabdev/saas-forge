-- ADR-006 Amendment 4: the delivery audience may now FILTER the snapshot.
--
-- For that audience the repository evaluates every caller-supplied predicate
-- against published_payload instead of payload (ListEntriesFilter.MatchPublished),
-- and the containment operators it emits — `published_payload @> $n` for
-- eq/has — are served by a jsonb_path_ops GIN index on that column or by
-- nothing: idx_entries_payload_gin (000010) covers the working copy only.
-- Same shape as 000010 on purpose; jsonb_path_ops is the smaller variant that
-- serves @> and nothing else, which is all the filter grammar emits.
--
-- Not CONCURRENTLY: the ledger applies each migration inside a transaction
-- (internal/platform/migrate), which CREATE INDEX CONCURRENTLY refuses.
CREATE INDEX IF NOT EXISTS idx_entries_published_payload_gin
    ON entries USING GIN (published_payload jsonb_path_ops);
