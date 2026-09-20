-- Scheduled publish / unpublish: an intent recorded now, executed later
-- (ADR-017).
--
-- WHAT THIS TABLE IS. One row is "at run_at, put entry X into state Y, on the
-- authority of the person who asked and against the version they were looking
-- at". It is an INTENT, not a queue of work items: the authorisation, the
-- target state and the version were all settled at request time, and the worker
-- that picks the row up later re-checks the version but does NOT re-decide any
-- of the three. ADR-017 §4 argues why, and the pinned_version column below is
-- the mechanism that makes "the approved version, not whatever the draft has
-- drifted into" enforceable rather than aspirational.
--
-- WHY NOT REUSE integration_outbox. That table is a delivery queue with retry
-- semantics: it exists to make an already-decided fact reach a consumer, and it
-- retries until it does. This one is the opposite shape. A schedule that comes
-- due may legitimately decide NOT to act (the working copy moved on), and a
-- failure here is a content operation that did not happen — a thing an editor
-- must see and re-decide, not a thing a retry loop should keep attempting
-- behind their back. Sharing a table would have forced one set of semantics
-- onto both.
--
-- WHY NOT A COLUMN ON `entries`. `entries.publish_at` would answer "when" and
-- nothing else: not who asked, not against which version, not what happened
-- when the moment arrived, and not the difference between "cancelled" and
-- "never scheduled". Those are exactly the questions the editor asks the day a
-- post did not go live, and a nullable timestamp cannot answer any of them.

CREATE TABLE IF NOT EXISTS content_entry_schedules (
    id              UUID PRIMARY KEY,
    tenant_id       TEXT NOT NULL,

    entry_id        UUID NOT NULL,

    -- Denormalised from `entries`, and deliberately so. Every read path for an
    -- entry in this codebase is keyed (tenant_id, content_type_id, id) — that
    -- is GetEntry's signature and the shape RLS and the indexes are built for.
    -- The worker holds only a schedule row, so without this column it would
    -- need either a second lookup that maps an entry to its type (a query no
    -- other caller wants, on a path where a missing row is ambiguous) or a
    -- widened GetEntry that drops the type from the key. Copying one immutable
    -- id is cheaper than either: an entry cannot change content type — no code
    -- path updates entries.content_type_id — so the copy cannot go stale.
    content_type_id UUID NOT NULL,

    -- 'publish' | 'unpublish'. The same two operations the immediate API
    -- offers, on purpose: a schedule must never be able to express a state
    -- change that a person could not perform directly, or it becomes a way
    -- around the authz split (content:publish vs content:update).
    action          TEXT NOT NULL,

    run_at          TIMESTAMPTZ NOT NULL,

    -- THE VERSION THE REQUESTER WAS LOOKING AT. This is the load-bearing
    -- column of the whole feature and the place it diverges from Strapi, which
    -- publishes whatever the draft happens to be at the scheduled moment.
    --
    -- ADR-014 §1 keeps publish as a human gate — a person's judgement on a
    -- specific piece of content. A schedule that published "the draft, whatever
    -- it says by then" would launder that judgement: approve version 7, let an
    -- agent write version 8 at 03:00, and version 8 goes live at 09:00 with
    -- nobody's approval on it. Pinning turns that into a refusal (state
    -- 'stale') instead of a silent publish, which is the failure mode this
    -- table is willing to have.
    --
    -- For an unpublish the value is recorded but not enforced as a gate on
    -- content — see ADR-017 §3; the re-check still runs, because "the draft
    -- moved" is information the editor wants either way.
    pinned_version  INT NOT NULL,

    -- pending → done | stale | cancelled | superseded | failed. All five exits
    -- are terminal; nothing re-enters 'pending'. A stale schedule is NOT
    -- auto-rescheduled, and that is the point: the editor approved a version,
    -- that version is gone, and only a person can approve the next one.
    --
    -- There is no 'running' state and no claimed_at. The claim is the row lock
    -- taken by the worker's `SELECT ... FOR UPDATE SKIP LOCKED` inside the same
    -- transaction that performs the publish and writes the terminal state, so a
    -- worker that dies mid-flight rolls back to 'pending' rather than stranding
    -- a row in a state only a reclaim job could clear. This is why 000011's
    -- claimed_at/ReclaimStale machinery has no counterpart here.
    state           TEXT NOT NULL DEFAULT 'pending',

    -- Who answers for the publish that happens later. The worker stamps this
    -- into entries.published_by, so provenance names the person who approved
    -- the release, never the process that carried it out. There is no service
    -- actor here and no agent actor: ADR-014 §1's gate means only a human
    -- reaches this table (agent_gate.go does not list content:publish, and
    -- ADR-017 §6 declines to add an MCP tool for scheduling).
    requested_by    UUID NOT NULL,

    created_at      TIMESTAMPTZ NOT NULL DEFAULT now(),

    -- Set only when the worker actually ran the operation — see the CHECK. A
    -- row that went 'stale', 'cancelled' or 'superseded' has no execution
    -- instant because nothing executed, and leaving the column NULL says that
    -- rather than pretending the moment it was marked was the moment it ran.
    executed_at     TIMESTAMPTZ,

    -- Empty string rather than NULL, matching content_activity.error_code
    -- (000032). The reason is the biconditional CHECK below: with a nullable
    -- column `(state = 'failed') = (error IS NOT NULL)` is a perfectly good
    -- constraint, but `error` would then have two spellings for "no error"
    -- (NULL and '') and every reader would have to handle both.
    error           TEXT NOT NULL DEFAULT '',

    CONSTRAINT content_entry_schedules_action_check
        CHECK (action IN ('publish', 'unpublish')),

    CONSTRAINT content_entry_schedules_state_check
        CHECK (state IN ('pending', 'done', 'stale', 'cancelled', 'superseded', 'failed')),

    -- A version is a positive counter (entries.version starts at 1), so 0 or a
    -- negative here is a caller that forgot to read the entry first.
    CONSTRAINT content_entry_schedules_pinned_version_check
        CHECK (pinned_version > 0),

    -- Executed exactly when it ran. Both directions matter: a 'done' row with
    -- no instant is an audit hole, and a 'cancelled' row carrying one claims an
    -- execution that never happened.
    CONSTRAINT content_entry_schedules_executed_at_check
        CHECK ((state IN ('done', 'failed')) = (executed_at IS NOT NULL)),

    -- Only a failure carries a message, and every failure carries one. Written
    -- as the biconditional for the same reason 000032 gave for error_code: a
    -- one-way check lets a failure with no cause recorded through, which is the
    -- half that is actually useless.
    CONSTRAINT content_entry_schedules_error_check
        CHECK ((state = 'failed') = (error <> '')),

    -- Deleting an entry takes its schedules with it. Unlike entry_revisions'
    -- cascade this discards nothing anyone can want: a schedule for a row that
    -- no longer exists could only ever resolve to a failure, and the record of
    -- what was scheduled and by whom already lives in content_activity, which
    -- does NOT cascade.
    FOREIGN KEY (entry_id) REFERENCES entries (id) ON DELETE CASCADE,

    FOREIGN KEY (content_type_id) REFERENCES content_types (id) ON DELETE CASCADE
);

-- ONE PENDING SCHEDULE PER ENTRY, enforced by the database rather than by a
-- read-then-write in the service.
--
-- The service does check first, and returns CONTENT_SCHEDULE_EXISTS with the
-- existing run_at so the editor sees a useful message instead of a constraint
-- name. But two concurrent requests both pass that check, and without this
-- index they would both insert — leaving an entry with two pending schedules
-- and no defined answer for which one wins. The partial index makes the loser's
-- INSERT fail, and the service maps the unique violation onto the same
-- CONTENT_SCHEDULE_EXISTS. Terminal rows are excluded from the index, so an
-- entry accumulates history freely; only the live intent is unique.
CREATE UNIQUE INDEX IF NOT EXISTS uq_content_entry_schedules_pending
    ON content_entry_schedules (entry_id)
    WHERE state = 'pending';

-- The worker's only query: "what is due". Partial on state so the index holds
-- the pending rows alone — the table is append-mostly and the pending set is
-- tiny beside it, which is what keeps a poll every 30s from degrading as
-- history accumulates.
--
-- Deliberately NOT led by tenant_id: this scan is cross-tenant by design (one
-- worker serves every tenant, as the outbox worker does), so a tenant-leading
-- index would never be used by the one reader it exists for.
CREATE INDEX IF NOT EXISTS idx_content_entry_schedules_due
    ON content_entry_schedules (run_at)
    WHERE state = 'pending';

-- The admin read path: "the latest schedule for this entry, whatever state it
-- is in" (GET /entries/{id}/schedule). Tenant-leading here because that read
-- IS tenant-scoped and goes through RLS.
CREATE INDEX IF NOT EXISTS idx_content_entry_schedules_entry_recent
    ON content_entry_schedules (tenant_id, entry_id, created_at DESC);

-- Same two-layer isolation as every content table (000014). SELECT / INSERT /
-- UPDATE have policies; DELETE has none, so the only removal is the cascade
-- from a deleted entry — a schedule is cancelled by moving it to a terminal
-- state, never by erasing the fact that it existed.
ALTER TABLE content_entry_schedules ENABLE ROW LEVEL SECURITY;
ALTER TABLE content_entry_schedules FORCE ROW LEVEL SECURITY;
DROP POLICY IF EXISTS content_entry_schedules_tenant_read ON content_entry_schedules;
CREATE POLICY content_entry_schedules_tenant_read ON content_entry_schedules
    FOR SELECT USING (tenant_id = app_current_tenant());
DROP POLICY IF EXISTS content_entry_schedules_tenant_insert ON content_entry_schedules;
CREATE POLICY content_entry_schedules_tenant_insert ON content_entry_schedules
    FOR INSERT WITH CHECK (tenant_id = app_current_tenant());
DROP POLICY IF EXISTS content_entry_schedules_tenant_update ON content_entry_schedules;
CREATE POLICY content_entry_schedules_tenant_update ON content_entry_schedules
    FOR UPDATE USING (tenant_id = app_current_tenant())
    WITH CHECK (tenant_id = app_current_tenant());

-- THE ONE DELIBERATE WIDENING, and the only place in the content plane where
-- anything but `app.tenant_id` can unlock a row.
--
-- The worker has to answer "which schedules across ALL tenants are due", and
-- 000014's model has no spelling for that: app_current_tenant() fails closed
-- when the GUC is unset, so a cross-tenant SELECT under a non-superuser role
-- returns zero rows. It would return rows in dev and CI — those connect as the
-- postgres superuser, which bypasses RLS — so the failure would be invisible
-- until production, where the symptom is "scheduled posts silently never go
-- live". That is the worst possible place to discover it.
--
-- So the scan opts in explicitly, with a second GUC the app sets ONLY in
-- ListDueSchedules (scheduler/repository), and the widening is scoped as
-- tightly as it can be made:
--   * this table only — no other content table has a policy naming this GUC;
--   * SELECT only — the execute phase re-enters under an ordinary
--     WithTx(tenantID) and is subject to the tenant policies above, so nothing
--     is WRITTEN under the wide GUC;
--   * and the rows it exposes carry ids, timestamps and a state — no payload,
--     no title, no tenant content of any kind. The worst a leak buys is the
--     knowledge that some tenant scheduled something.
--
-- ⚠️ The GUC is at the same trust level as `app.tenant_id` itself: anything
-- that can set one can set the other, so this is not a new attack surface, it
-- is the same one named differently. What it must never become is a general
-- "admin mode" — the moment a second table names app_scheduler_scan(), the
-- fail-closed property that makes 000014 worth having starts leaking away one
-- policy at a time. Add a purpose-specific GUC instead.
CREATE OR REPLACE FUNCTION app_scheduler_scan() RETURNS BOOLEAN
    LANGUAGE sql STABLE AS $$ SELECT current_setting('app.scheduler_scan', true) = 'on' $$;

DROP POLICY IF EXISTS content_entry_schedules_scheduler_scan ON content_entry_schedules;
CREATE POLICY content_entry_schedules_scheduler_scan ON content_entry_schedules
    FOR SELECT USING (app_scheduler_scan());

-- HOW A SCHEDULED PUBLISH IS TOLD APART FROM A HAND-PRESSED ONE.
--
-- The worker records the ordinary content:publish / content:unpublish activity
-- (ADR-014 §3) so a release reaches the activity stream whether a person
-- pressed the button or a schedule came due — an "activity" log that omits half
-- the publishes would be worse than none. But the two must remain
-- distinguishable, or "who published this at 3am" reads as a person who was
-- asleep.
--
-- 000032 refused a payload/details column and that refusal stands: these two
-- columns name the MECHANISM, not the content. `via` is a closed vocabulary
-- (CHECK), not free text, so it cannot become the details column by drift.
ALTER TABLE content_activity
    ADD COLUMN IF NOT EXISTS via TEXT NOT NULL DEFAULT '';
ALTER TABLE content_activity
    ADD COLUMN IF NOT EXISTS via_schedule_id UUID;

-- details carries STRUCTURED FACTS ABOUT THE ACTION — never content values.
-- The type comment on domain.Activity draws that line for changed_keys ("keys
-- and never values"), and it is the same line: the moment a payload value lands
-- here, this table has become a second version history with none of the first's
-- retention rules and no field-level masking.
--
-- It exists because entry.schedule.stale has three facts to carry (which
-- schedule died, the version it was pinned to, the version that killed it) and
-- nowhere to put them. The alternatives were worse: via_schedule_id is fenced
-- by the biconditional below and would have to be un-fenced, and changed_keys
-- is a TEXT[] of key names that a version number is not.
--
-- NULL, not '{}', when there is nothing to say: a reader must be able to tell
-- "this action carries no structured detail" from "it carries an empty one".
ALTER TABLE content_activity
    ADD COLUMN IF NOT EXISTS details JSONB;

ALTER TABLE content_activity
    DROP CONSTRAINT IF EXISTS content_activity_via_check;
ALTER TABLE content_activity
    ADD CONSTRAINT content_activity_via_check
    CHECK (via IN ('', 'schedule'));

-- The biconditional, for 000032's reason: a 'schedule' row with no schedule id
-- cannot be traced back, and a schedule id on a hand-pressed row is a false
-- claim about how the publish happened.
ALTER TABLE content_activity
    DROP CONSTRAINT IF EXISTS content_activity_via_schedule_id_check;
ALTER TABLE content_activity
    ADD CONSTRAINT content_activity_via_schedule_id_check
    CHECK ((via = 'schedule') = (via_schedule_id IS NOT NULL));

-- No FOREIGN KEY to content_entry_schedules, on purpose. Activity is
-- append-only history and must outlive the thing it describes; a real FK would
-- either cascade the history away when an entry is deleted or block the delete
-- outright. The id is a breadcrumb, and a reader that cannot resolve it has
-- learned the true thing — the schedule is gone — rather than been lied to.
