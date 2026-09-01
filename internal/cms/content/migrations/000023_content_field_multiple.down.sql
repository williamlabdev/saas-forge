-- Reverses 000023. Field definitions survive; they revert to single-valued.
--
-- Note what this does NOT do: entries whose payload already holds arrays keep
-- holding them, and they will fail validation on their next write against the
-- reverted schema. A rollback here is only safe before any field actually
-- adopted the flag — the same asymmetry every column-drop has, stated rather
-- than left to be discovered.

ALTER TABLE content_type_fields
    DROP COLUMN IF EXISTS multiple;
