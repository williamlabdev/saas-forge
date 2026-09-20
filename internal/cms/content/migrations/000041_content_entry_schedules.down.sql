-- Reverses 000041.
--
-- WHAT IS LOST. Every pending schedule, silently: after this runs, an entry an
-- editor scheduled for Monday simply never goes live, and nothing anywhere
-- records that it was ever going to. There is no degraded mode to fall back on
-- — a schedule with no table and no worker is not a late publish, it is a
-- publish that will not happen. Whoever runs this owes the tenants with pending
-- rows a heads-up, and the rows are readable right up until the DROP.
--
-- WHAT SURVIVES. content_activity keeps every schedule/cancel/publish row it
-- recorded; only the `via` columns go, so a scheduled publish becomes
-- indistinguishable from a hand-pressed one rather than disappearing. That is
-- the right thing to lose last: the history of what happened outlives the
-- mechanism that made it happen.

-- Table first: its scan policy depends on the function below, and dropping the
-- table takes the policy, the indexes and the constraints with it.
DROP TABLE IF EXISTS content_entry_schedules;

DROP FUNCTION IF EXISTS app_scheduler_scan;

-- Constraints before columns. Dropping the columns would take these with them,
-- but naming them keeps the file honest about everything it removes — the
-- rollback test reads this text as the file's own claim about what it does.
ALTER TABLE content_activity
    DROP CONSTRAINT IF EXISTS content_activity_via_schedule_id_check;
ALTER TABLE content_activity
    DROP CONSTRAINT IF EXISTS content_activity_via_check;
ALTER TABLE content_activity
    DROP COLUMN IF EXISTS via_schedule_id;
ALTER TABLE content_activity
    DROP COLUMN IF EXISTS via;
-- The stale lines keep their action verb and lose only the three facts they carried; an
-- operator can still see THAT a schedule was invalidated, not which one.
ALTER TABLE content_activity
    DROP COLUMN IF EXISTS details;
