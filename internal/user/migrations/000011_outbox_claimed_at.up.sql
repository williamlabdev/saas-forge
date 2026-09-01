-- TKT-OBX-1: record when a row was claimed so a stale-processing reaper can
-- reclaim rows that were claimed but never marked done/failed (worker crash or
-- shutdown mid-batch). Without this timestamp, 'processing' rows are a silent
-- black hole.
ALTER TABLE integration_outbox ADD COLUMN claimed_at TIMESTAMPTZ;

-- Supports the reaper's "processing older than threshold" scan.
CREATE INDEX idx_integration_outbox_processing ON integration_outbox (claimed_at)
    WHERE status = 'processing';
