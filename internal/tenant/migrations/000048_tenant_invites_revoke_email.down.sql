ALTER TABLE tenant_invites
    DROP CONSTRAINT IF EXISTS tenant_invites_accept_revoke_exclusive,
    DROP CONSTRAINT IF EXISTS tenant_invites_revoked_consistency,
    DROP CONSTRAINT IF EXISTS tenant_invites_email_nonce_pair,
    DROP COLUMN IF EXISTS revoked_by,
    DROP COLUMN IF EXISTS revoked_at,
    DROP COLUMN IF EXISTS email_encrypted_nonce,
    DROP COLUMN IF EXISTS email_encrypted;
