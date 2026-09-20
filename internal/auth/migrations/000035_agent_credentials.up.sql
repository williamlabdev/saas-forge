-- Agent credentials: the registry that makes an agent token REVOCABLE
-- (ADR-013, ruled 2026-08-06).
--
-- WHY A TABLE AT ALL, when the token is a signed JWT that needs no lookup to
-- verify. Because a JWT is valid until it expires and nothing anyone decides
-- afterwards changes that. With the 15-minute access TTL an agent inherited
-- before today, that was survivable — the credential died on its own faster
-- than anyone could react. The ruling replaced it with a long TTL precisely so
-- an unattended agent stops needing a human login every quarter hour, and a
-- 30-day bearer token with no off switch is a different, worse thing. This
-- table is the off switch: the middleware looks the row up by the token's `jti`
-- and refuses the token when the row is missing, revoked or expired.
--
-- SO THE ROW IS THE AUTHORITY AND THE SIGNATURE IS NOT. A row that is absent is
-- a refusal, never a pass — which is what makes the ON DELETE CASCADE below a
-- feature rather than a hazard.
--
-- WHAT IS DELIBERATELY NOT HERE: the token, or a hash of it. refresh_tokens
-- stores token_hash because a refresh token is an opaque string with no other
-- proof of authenticity; here the signature already proves the token, and the
-- row exists only to say whether it is still allowed to work. Storing a hash
-- would add a second thing to keep in step with the first and buy nothing.
CREATE TABLE agent_credentials (
    -- The id IS the token's `jti` claim. One credential, one row, and the token
    -- carries the pointer to its own kill switch.
    id            UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    -- Copied off the minter's claims, never named by the caller: IssueAgentToken
    -- takes TenantID from the minter, so there is no request field that could
    -- ask for another tenant. This column is what every read scopes on.
    tenant_id     TEXT NOT NULL,
    -- The agent's own name, as it appears in provenance (ADR-013 §2). Not
    -- unique: minting a second credential for the same agent is how rotation
    -- works before there is a rotate verb — mint, switch over, revoke the old.
    agent_id      TEXT NOT NULL,
    -- The person answerable for it. ON DELETE CASCADE is the ruling, not a
    -- default: when the person is gone their unattended agents stop, and
    -- because an absent row is a refusal, deleting the user IS revoking every
    -- credential they minted. There is no window where a departed employee's
    -- agent keeps writing.
    principal_id  UUID NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    -- The tenant role the credential carries, recorded so the list can say what
    -- a credential can reach. It is NOT read back when the token is verified —
    -- the token carries its own role claim, and a second copy that could
    -- disagree with the signed one would be a way to widen a credential after
    -- it was signed.
    tenant_role   TEXT NOT NULL,
    -- The content-type whitelist (ADR-013 §1/§4), same purpose: shown, not
    -- enforced from here.
    --
    -- The CHECK is the database's copy of ErrAgentScopeUnset — "not configured"
    -- must never read as "everything". It is not redundant decoration: the
    -- signer's version guards the one path that exists today, and this one
    -- guards every INSERT that will ever exist, including a backfill or a
    -- fixture that never goes near IssueAgentToken.
    allowed_types TEXT[] NOT NULL CHECK (cardinality(allowed_types) > 0),
    expires_at    TIMESTAMPTZ NOT NULL,
    revoked_at    TIMESTAMPTZ,
    -- Who turned it off. No FK on purpose: this is a history record, and it must
    -- survive the revoker's account being deleted. An FK with ON DELETE SET NULL
    -- would silently turn "admin X stopped this agent" into "nobody did", and
    -- the CHECK below would then be violated by a row that was correct when
    -- written.
    revoked_by    UUID,
    created_at    TIMESTAMPTZ NOT NULL DEFAULT now(),
    -- Both NULL or both set. Written as an equality between two IS NULL tests
    -- rather than as `revoked_at IS NULL OR revoked_by IS NOT NULL`, because the
    -- latter shape evaluates to NULL on the interesting row and a CHECK that
    -- evaluates to NULL PASSES (the trap 000030 walked into).
    CONSTRAINT agent_credentials_revocation_complete
        CHECK ((revoked_at IS NULL) = (revoked_by IS NULL))
);

-- Listing a tenant's credentials, newest first. The per-request check goes
-- through the primary key and needs no index of its own.
CREATE INDEX idx_agent_credentials_tenant ON agent_credentials (tenant_id, created_at DESC);
