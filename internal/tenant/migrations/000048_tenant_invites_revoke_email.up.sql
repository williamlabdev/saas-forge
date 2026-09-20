-- 2.2c 成員管理 API (ADR-022): pending invites need a reversibly-decryptable
-- email to display in GET /tenants/invites (email_lookup_hash alone is a
-- one-way HMAC and cannot be shown back), plus a revocation marker so
-- DELETE /tenants/invites/{id} can cancel an invite before it is accepted.
--
-- Nullable/paired the same way users.display_name_encrypted is (see
-- users_display_name_nonce_pair, internal/user/migrations/000001): every
-- EXISTING row predates this column and has no plaintext to re-encrypt, so
-- both new email columns stay nullable and NEW rows populate them going
-- forward (see tenant_service.go CreateInvite).
ALTER TABLE tenant_invites
    ADD COLUMN email_encrypted       BYTEA,
    ADD COLUMN email_encrypted_nonce BYTEA,
    ADD COLUMN revoked_at            TIMESTAMPTZ,
    ADD COLUMN revoked_by            UUID REFERENCES users (id);

ALTER TABLE tenant_invites
    ADD CONSTRAINT tenant_invites_email_nonce_pair CHECK (
        (email_encrypted IS NULL) = (email_encrypted_nonce IS NULL)
    ),
    ADD CONSTRAINT tenant_invites_revoked_consistency CHECK (
        (revoked_at IS NULL) = (revoked_by IS NULL)
    );

-- An invite that is both accepted and revoked would be a data bug (accept and
-- revoke are meant to be mutually exclusive terminal states); this constraint
-- makes that state unrepresentable rather than merely undesired.
ALTER TABLE tenant_invites
    ADD CONSTRAINT tenant_invites_accept_revoke_exclusive CHECK (
        accepted_at IS NULL OR revoked_at IS NULL
    );
