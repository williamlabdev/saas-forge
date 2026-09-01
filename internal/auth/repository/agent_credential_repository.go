package repository

import (
	"context"
	"errors"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// AgentCredential is one minted agent credential as the tenant sees it. The
// token itself is not here and never will be: it is returned once, at mint
// time, and after that only its id is knowable.
type AgentCredential struct {
	ID           uuid.UUID
	TenantID     string
	AgentID      string
	PrincipalID  uuid.UUID
	TenantRole   string
	AllowedTypes []string
	ExpiresAt    time.Time
	RevokedAt    *time.Time
	RevokedBy    *uuid.UUID
	CreatedAt    time.Time
}

// ErrAgentCredentialNotFound is returned when no credential in the caller's
// tenant has the given id. It does NOT distinguish "no such credential" from
// "a credential belonging to another tenant" — that distinction is exactly the
// cross-tenant probe this repository refuses to answer.
var ErrAgentCredentialNotFound = errors.New("auth: agent credential not found")

// AgentCredentialRepository is the registry behind agent-token revocation
// (ADR-013, ruled 2026-08-06).
type AgentCredentialRepository interface {
	Insert(ctx context.Context, cred AgentCredential) error
	// ListByTenant returns the tenant's credentials, newest first, revoked and
	// expired ones INCLUDED. A list that showed only live credentials would be
	// unable to answer "was this one already turned off?", which is the first
	// question anyone asks when they come here.
	ListByTenant(ctx context.Context, tenantID string) ([]AgentCredential, error)
	// Revoke turns a credential off. Scoped by tenant in the WHERE clause, not
	// by a check in the service: this is the only thing standing between an
	// owner of tenant A and a credential id belonging to tenant B.
	//
	// Revoking an already-revoked credential is NOT an error and does not move
	// revoked_at. The caller of a kill switch under incident conditions presses
	// it twice, and the honest answer to the second press is "it is off",
	// while rewriting the timestamp would lose when it actually stopped.
	Revoke(ctx context.Context, tenantID string, id uuid.UUID, revokedBy uuid.UUID) error
	// IsActive answers the per-request question: may the credential with this
	// id still be used, as of now. Missing, revoked and expired are all false,
	// and the caller cannot tell them apart — a bearer learns only "no".
	IsActive(ctx context.Context, id uuid.UUID) (bool, error)
}

// PostgresAgentCredentialRepository implements AgentCredentialRepository.
type PostgresAgentCredentialRepository struct {
	pool *pgxpool.Pool
}

func NewPostgresAgentCredentialRepository(pool *pgxpool.Pool) *PostgresAgentCredentialRepository {
	return &PostgresAgentCredentialRepository{pool: pool}
}

func (r *PostgresAgentCredentialRepository) Insert(ctx context.Context, cred AgentCredential) error {
	_, err := r.pool.Exec(ctx, `
		INSERT INTO agent_credentials
			(id, tenant_id, agent_id, principal_id, tenant_role, allowed_types, expires_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7)
	`, cred.ID, cred.TenantID, cred.AgentID, cred.PrincipalID, cred.TenantRole, cred.AllowedTypes, cred.ExpiresAt)
	return err
}

//nolint:gosec // G101: a SELECT column list, not a secret — the token is never stored, only the row id.
const agentCredentialColumns = `
	id, tenant_id, agent_id, principal_id, tenant_role, allowed_types,
	expires_at, revoked_at, revoked_by, created_at
`

func (r *PostgresAgentCredentialRepository) ListByTenant(ctx context.Context, tenantID string) ([]AgentCredential, error) {
	rows, err := r.pool.Query(ctx, `
		SELECT `+agentCredentialColumns+`
		FROM agent_credentials
		WHERE tenant_id = $1
		ORDER BY created_at DESC
	`, tenantID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	creds := make([]AgentCredential, 0)
	for rows.Next() {
		var c AgentCredential
		if err := rows.Scan(
			&c.ID, &c.TenantID, &c.AgentID, &c.PrincipalID, &c.TenantRole, &c.AllowedTypes,
			&c.ExpiresAt, &c.RevokedAt, &c.RevokedBy, &c.CreatedAt,
		); err != nil {
			return nil, err
		}
		creds = append(creds, c)
	}
	return creds, rows.Err()
}

func (r *PostgresAgentCredentialRepository) Revoke(ctx context.Context, tenantID string, id uuid.UUID, revokedBy uuid.UUID) error {
	// The WHERE clause deliberately does not exclude already-revoked rows: it
	// has to match them, or a second press would report "not found" and read as
	// "somebody deleted my credential". `revoked_at IS NULL` in the SET guard
	// is what keeps the original timestamp.
	tag, err := r.pool.Exec(ctx, `
		UPDATE agent_credentials
		SET revoked_at = COALESCE(revoked_at, now()),
		    revoked_by = COALESCE(revoked_by, $3)
		WHERE id = $1 AND tenant_id = $2
	`, id, tenantID, revokedBy)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return ErrAgentCredentialNotFound
	}
	return nil
}

func (r *PostgresAgentCredentialRepository) IsActive(ctx context.Context, id uuid.UUID) (bool, error) {
	// now() is the DATABASE's clock, not the process's. The signature already
	// bounds the token's lifetime against the signer's clock; this second
	// comparison exists so that a credential whose row says expired is refused
	// even if some future path issues a token whose exp disagrees with the row.
	var active bool
	err := r.pool.QueryRow(ctx, `
		SELECT revoked_at IS NULL AND expires_at > now()
		FROM agent_credentials
		WHERE id = $1
	`, id).Scan(&active)
	if errors.Is(err, pgx.ErrNoRows) {
		// The absent row is the CASCADE case among others: the person who minted
		// the credential was deleted, so the credential is gone with them. "No
		// row" must never fall through to a pass.
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return active, nil
}
