-- Idempotent entry creation: which key already produced which entry (ADR-013 §9).
--
-- WHY THIS IS A SECOND IMPLEMENTATION AND NOT A REUSE. internal/pkg/idempotency
-- exists and was listed as "wire up the existing package" in the first draft of
-- §9; §9 itself corrects that. Its every symbol is registration-specific
-- (RegistrationStore, UserIDByKey, RecordTx, registration_idempotency) and its
-- store takes a raw pgx.Tx, while the content repository hands out a
-- ContentRepository bound to a transaction (WithTx). What carries over is
-- NormalizeKey and the unique-violation shape; the rest is this table.
--
-- WHY ONLY CREATE. §9 says "entries idempotency", and the scope is narrower than
-- it reads: PATCH /entries/{id} already requires the caller's expected `version`
-- (the optimistic lock), and unpublish is idempotent in the status column
-- itself. A retried CREATE is the only one of the three that silently produces a
-- second row. So this table covers exactly that one statement, and a key sent to
-- any other endpoint is not a no-op that got ignored — no other endpoint reads
-- this table at all.

CREATE TABLE IF NOT EXISTS entry_idempotency (
    tenant_id  TEXT NOT NULL,

    -- WHO the key belongs to. Not just the tenant (william ruled 2026-08-06).
    --
    -- A replay returns a row by primary-key lookup, which is a read that does
    -- not pass through the projection and permission path a normal GET does. If
    -- the namespace were tenant-wide, caller B sending a key caller A had used
    -- would receive A's entry — on an own_only type that is a row B was refused,
    -- delivered by the write path. Scoping to the issuer means that shape does
    -- not exist rather than being guarded against.
    --
    -- The spelling is asymmetric between humans and agents, and the asymmetry is
    -- about ROTATION CADENCE, not about trust:
    --
    --   human:<user_id>       a person's access token dies of a 15-minute TTL.
    --                         Scoping to the token would drop the namespace
    --                         mid-retry every quarter hour, so it scopes to the
    --                         person, who is stable.
    --
    --   agent:<credential_id> an agent credential lives 30 days (補裁 O), long
    --                         enough that the tighter scope costs nothing. And
    --                         it must be tighter than the agent NAME: AgentID is
    --                         an arbitrary string chosen at mint time, so two
    --                         people in one tenant can both mint "writer-bot"
    --                         and would otherwise share this namespace — exactly
    --                         the collision the ruling removes.
    --
    -- An agent credential with no credential_id cannot appear (jwt sets it
    -- whenever Kind is agent); the service refuses the key rather than falling
    -- back to the user id, because that fallback would merge every agent of one
    -- minter into one namespace, which is the loose scope wearing the tight
    -- scope's name.
    actor_key  TEXT NOT NULL,

    idem_key   VARCHAR(128) NOT NULL,

    -- The request this key was FIRST used for. Same key, different request =>
    -- 409, never a silent replay (william ruled 2026-08-06).
    --
    -- This is the column that makes the table safe for an unattended writer. The
    -- failure it removes is silent: an agent that reuses a key by accident — a
    -- loop variable that did not advance, a resumed plan — sends new content,
    -- receives 201 with an OLD entry, and has no way to notice its work was
    -- never stored. Without a fingerprint the platform's answer to "did you save
    -- this?" is indistinguishable from yes.
    --
    -- WHAT IS HASHED, AND WHY THE FALSE ANSWER GOES THE SAFE WAY. The digest
    -- covers the whole request that shapes the row: type name, locale,
    -- translation_of, and the payload compacted (insignificant whitespace
    -- removed, key order untouched). Compaction is not canonicalisation, so a
    -- client that re-serialises the same document with different key order gets
    -- a 409 for a request that was in fact identical.
    --
    -- That direction is deliberate. A false MISMATCH is loud, arrives with a
    -- code that says what happened, and is fixed with a new key. A false MATCH
    -- would be the silent failure above, and hashing raw bytes cannot produce
    -- one: different requests always differ in the digest.
    fingerprint BYTEA NOT NULL,

    -- ON DELETE CASCADE, and it means the key is spent when the entry is gone: a
    -- replay after a hard delete creates a NEW entry rather than 404ing on a
    -- record pointing at nothing. That is the honest reading — the thing the key
    -- promised to return does not exist — and it is the only option that keeps
    -- the reference valid at all.
    entry_id   UUID NOT NULL REFERENCES entries (id) ON DELETE CASCADE,

    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),

    PRIMARY KEY (tenant_id, actor_key, idem_key)
);

-- RETENTION IS UNRULED AND THEREFORE ABSENT (ADR-013 未解項).
--
-- These rows are small and never read after the retry window closes — hours at
-- most — so a purge is the obvious follow-up. It is not written here because
-- what a purge changes is USER-VISIBLE, not just storage: the day a key expires
-- is the day a replay of it creates a second entry instead of returning the
-- first. That is a ruling about how long the promise holds, and 000034's
-- retention note is the precedent for not inventing one inside a migration.
--
-- created_at carries no index for the same reason: it exists so a purge can be
-- written against it, and an unread index is pure write cost until then.

-- Same two-layer isolation as every content table (000014): the app scopes by
-- tenant_id, RLS refuses what a forgotten WHERE would leak. FORCE so the owner
-- is subject too.
--
-- SELECT and INSERT only, as 000032 and 000034. Nothing rewrites a record: a key
-- names one request and one outcome for its whole life, and an UPDATE here would
-- be the mechanism for pointing a key that a caller already holds at different
-- content. The cascade above is not an exception — it fires from the referenced
-- row's deletion, not from a DELETE aimed at this table.
ALTER TABLE entry_idempotency ENABLE ROW LEVEL SECURITY;
ALTER TABLE entry_idempotency FORCE ROW LEVEL SECURITY;
DROP POLICY IF EXISTS entry_idempotency_tenant_read ON entry_idempotency;
CREATE POLICY entry_idempotency_tenant_read ON entry_idempotency
    FOR SELECT USING (tenant_id = app_current_tenant());
DROP POLICY IF EXISTS entry_idempotency_tenant_insert ON entry_idempotency;
CREATE POLICY entry_idempotency_tenant_insert ON entry_idempotency
    FOR INSERT WITH CHECK (tenant_id = app_current_tenant());
