-- The activity record: who did what to which thing, and did it work (ADR-014 §3).
--
-- This is the table that makes the ADR's dominating rule enforceable — "anything
-- an agent can do, the console must be able to say afterwards what it did". Two
-- properties of its shape follow directly from that rule and are not stylistic:
--
--   * REFUSALS ARE ROWS, first-class beside successes. An agent repeatedly
--     hitting a permission wall is the strongest available signal of what it is
--     trying to do, and until now the 403 went to the agent and nowhere else.
--     Hence outcome + error_code rather than a table of things that worked.
--
--   * NO PAYLOAD COLUMN. changed_keys names keys and never values. That is the
--     division of labour with §5's revisions (this answers "who did what", those
--     answer "what the content became"), and it is also what stops this from
--     growing into a second version history with none of the first's retention
--     rules — and from becoming a copy of every restricted field's value in a
--     table that has no field-level masking. target_title is the one
--     denormalised scrap of content and it is fenced in Go: domain.TitleFor
--     refuses to source a label from a read-restricted field, for exactly that
--     reason.

CREATE TABLE IF NOT EXISTS content_activity (
    id              UUID PRIMARY KEY,
    tenant_id       TEXT NOT NULL,
    occurred_at     TIMESTAMPTZ NOT NULL DEFAULT now(),

    -- The actor trio (§4). actor_user_id is who ANSWERS for the action, not
    -- necessarily who typed: for an agent credential it is the principal who
    -- minted it. NULL means there is no person to name — a delivery or preview
    -- credential — which a reader must render as the service it is.
    actor_kind      TEXT NOT NULL,
    actor_user_id   UUID,
    actor_agent_id  TEXT,

    action          TEXT NOT NULL,

    -- target_type is empty for actions concerning no single content type.
    -- target_title is denormalised at write time on purpose: the entry may be
    -- deleted, and a stream of bare uuids is one nobody reads.
    target_type     TEXT NOT NULL DEFAULT '',
    target_entry_id UUID,
    target_title    TEXT NOT NULL DEFAULT '',

    outcome         TEXT NOT NULL,
    error_code      TEXT NOT NULL DEFAULT '',

    changed_keys    TEXT[] NOT NULL DEFAULT '{}',

    CONSTRAINT content_activity_actor_kind_check
        CHECK (actor_kind IN ('human', 'agent', 'service')),

    -- The biconditional from 000030/000031, and here it may be written the
    -- SHORT way: actor_kind is NOT NULL, so both sides are decidable and the
    -- three-valued gap 000031 had to spell around cannot open. That column is
    -- NOT NULL because this table has no history — every row is written by the
    -- recorder, which always knows the kind — so there is no population of
    -- rows for which "not recorded" is the honest answer. 000031's nullable
    -- column was the opposite case and its comment says why.
    CONSTRAINT content_activity_actor_agent_kind_check
        CHECK ((actor_kind = 'agent') = (actor_agent_id IS NOT NULL)),

    -- An agent line names the principal who answers for it. A bot acting with
    -- nobody accountable is the shape ADR-013 §2 exists to keep out.
    CONSTRAINT content_activity_agent_has_principal_check
        CHECK (actor_kind <> 'agent' OR actor_user_id IS NOT NULL),

    CONSTRAINT content_activity_outcome_check
        CHECK (outcome IN ('success', 'denied')),

    -- A refusal names its code and a success has none. Without both halves the
    -- stream can hold a "denied" line that does not say what refused it, which
    -- is the one thing a reader of a refusal needs.
    CONSTRAINT content_activity_error_code_check
        CHECK ((outcome = 'denied') = (error_code <> '')),

    -- A refusal changed nothing. A denied row carrying changed keys would be
    -- claiming an effect that did not happen.
    CONSTRAINT content_activity_denied_changes_nothing_check
        CHECK (outcome = 'success' OR cardinality(changed_keys) = 0),

    -- A title with no entry to hang it on is a label for nothing.
    CONSTRAINT content_activity_title_needs_entry_check
        CHECK (target_title = '' OR target_entry_id IS NOT NULL)
);

-- `action` is deliberately NOT pinned to an enum here.
--
-- The vocabulary grows with the agent tool surface by design (ADR-014's
-- dominating rule makes a new tool REQUIRE a new action), so a CHECK would add
-- a migration to every tool without adding a guarantee: the guard that the
-- vocabulary keeps up is ADR-014 §驗證計畫第 6 條's structural test, whose
-- yardstick is ADR-013 §5's tool list. A hand-maintained CHECK would be a
-- second list to remember, and the one that fails loudly at 3am instead of in
-- CI. Emptiness is still refused, because an unnamed action is not a record.
ALTER TABLE content_activity
    DROP CONSTRAINT IF EXISTS content_activity_action_present_check;
ALTER TABLE content_activity
    ADD CONSTRAINT content_activity_action_present_check
    CHECK (action <> '');

-- The console's query: this tenant's stream, newest first.
CREATE INDEX IF NOT EXISTS idx_content_activity_tenant_time
    ON content_activity (tenant_id, occurred_at DESC);

-- §1's per-field author attribution on the release screen (step 4) asks a
-- narrower question: what happened to THIS entry since it was last published.
-- Partial, because the rows with no entry — type reads, schema verbs — are
-- never an answer to it.
CREATE INDEX IF NOT EXISTS idx_content_activity_entry_time
    ON content_activity (tenant_id, target_entry_id, occurred_at DESC)
    WHERE target_entry_id IS NOT NULL;

-- Same two-layer isolation as every content table (000014): the app scopes by
-- tenant_id, RLS refuses what a forgotten WHERE would leak. FORCE so the table
-- owner is subject too.
--
-- APPEND-ONLY IS EXPRESSED BY OMISSION: there are policies FOR SELECT and FOR
-- INSERT and deliberately none for UPDATE or DELETE, so under a non-superuser
-- role those statements match no policy and reach no row. Stated honestly, that
-- is defence-in-depth and not the guarantee itself — 000014's note applies here
-- too, a superuser connection bypasses RLS entirely and the dev compose is one.
-- The primary guarantee is that no code path updates or deletes these rows;
-- this is what stops a future one from doing it quietly.
ALTER TABLE content_activity ENABLE ROW LEVEL SECURITY;
ALTER TABLE content_activity FORCE ROW LEVEL SECURITY;
DROP POLICY IF EXISTS content_activity_tenant_read ON content_activity;
CREATE POLICY content_activity_tenant_read ON content_activity
    FOR SELECT USING (tenant_id = app_current_tenant());
DROP POLICY IF EXISTS content_activity_tenant_insert ON content_activity;
CREATE POLICY content_activity_tenant_insert ON content_activity
    FOR INSERT WITH CHECK (tenant_id = app_current_tenant());
