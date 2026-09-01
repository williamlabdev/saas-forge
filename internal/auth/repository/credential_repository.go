package repository

import (
	"context"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// CredentialRepository persists credentials and refresh tokens.
type CredentialRepository interface {
	InsertCredentialsTx(ctx context.Context, tx pgx.Tx, userID uuid.UUID, passwordHash string) error
	UpdateLastLogin(ctx context.Context, userID uuid.UUID) error
	UserIDByEmailLookup(ctx context.Context, emailLookupHash []byte) (uuid.UUID, error)
	GetPasswordHash(ctx context.Context, userID uuid.UUID) (string, error)
	// StoreRefreshToken records the active tenant the token was issued for
	// (F2); empty tenantSlug is stored as NULL (pre-tenant / no-membership).
	StoreRefreshToken(ctx context.Context, userID uuid.UUID, tokenHash string, expiresAt time.Time, tenantSlug string) error
	RevokeRefreshToken(ctx context.Context, tokenHash string) error
	// RevokeAllForUser revokes every un-revoked refresh token a user holds.
	// Called when an account leaves the active state (soft delete today,
	// suspension when an admin path for it exists): changing the status column
	// alone would leave already-issued tokens minting new sessions.
	RevokeAllForUser(ctx context.Context, userID uuid.UUID) error
	// FindValidRefresh returns the owning user and the tenant slug the token
	// was issued for ("" when the row predates tenant tracking).
	FindValidRefresh(ctx context.Context, tokenHash string) (uuid.UUID, string, error)
	// UserStatusByID returns the account status ('active' / 'suspended' /
	// 'deleted'). The auth module reads the raw column rather than importing
	// user/domain, which would invert the module dependency.
	UserStatusByID(ctx context.Context, userID uuid.UUID) (string, error)
}

// PostgresCredentialRepository implements CredentialRepository.
type PostgresCredentialRepository struct {
	pool *pgxpool.Pool
}

func NewPostgresCredentialRepository(pool *pgxpool.Pool) *PostgresCredentialRepository {
	return &PostgresCredentialRepository{pool: pool}
}

func (r *PostgresCredentialRepository) InsertCredentialsTx(ctx context.Context, tx pgx.Tx, userID uuid.UUID, passwordHash string) error {
	_, err := tx.Exec(ctx, `
		INSERT INTO credentials (user_id, password_hash) VALUES ($1, $2)
	`, userID, passwordHash)
	return err
}

func (r *PostgresCredentialRepository) UserIDByEmailLookup(ctx context.Context, emailLookupHash []byte) (uuid.UUID, error) {
	var id uuid.UUID
	err := r.pool.QueryRow(ctx, `
		SELECT id FROM users WHERE email_lookup_hash = $1 AND status <> 'deleted'
	`, emailLookupHash).Scan(&id)
	return id, err
}

func (r *PostgresCredentialRepository) GetPasswordHash(ctx context.Context, userID uuid.UUID) (string, error) {
	var stored string
	err := r.pool.QueryRow(ctx, `SELECT password_hash FROM credentials WHERE user_id = $1`, userID).Scan(&stored)
	return stored, err
}

func (r *PostgresCredentialRepository) UpdateLastLogin(ctx context.Context, userID uuid.UUID) error {
	_, err := r.pool.Exec(ctx, `UPDATE credentials SET last_login = now() WHERE user_id = $1`, userID)
	return err
}

func (r *PostgresCredentialRepository) StoreRefreshToken(ctx context.Context, userID uuid.UUID, tokenHash string, expiresAt time.Time, tenantSlug string) error {
	_, err := r.pool.Exec(ctx, `
		INSERT INTO refresh_tokens (user_id, token_hash, expires_at, tenant_id)
		VALUES ($1, $2, $3, NULLIF($4, ''))
	`, userID, tokenHash, expiresAt, tenantSlug)
	return err
}

func (r *PostgresCredentialRepository) RevokeRefreshToken(ctx context.Context, tokenHash string) error {
	_, err := r.pool.Exec(ctx, `
		UPDATE refresh_tokens SET revoked_at = now() WHERE token_hash = $1 AND revoked_at IS NULL
	`, tokenHash)
	return err
}

func (r *PostgresCredentialRepository) RevokeAllForUser(ctx context.Context, userID uuid.UUID) error {
	_, err := r.pool.Exec(ctx, `
		UPDATE refresh_tokens SET revoked_at = now() WHERE user_id = $1 AND revoked_at IS NULL
	`, userID)
	return err
}

// UserStatusByID casts the enum to text so this query stays independent of the
// Go enum type generated for the user module.
func (r *PostgresCredentialRepository) UserStatusByID(ctx context.Context, userID uuid.UUID) (string, error) {
	var status string
	err := r.pool.QueryRow(ctx, `SELECT status::text FROM users WHERE id = $1`, userID).Scan(&status)
	return status, err
}

func (r *PostgresCredentialRepository) FindValidRefresh(ctx context.Context, tokenHash string) (uuid.UUID, string, error) {
	var userID uuid.UUID
	var tenantSlug string
	err := r.pool.QueryRow(ctx, `
		SELECT user_id, COALESCE(tenant_id, '') FROM refresh_tokens
		WHERE token_hash = $1 AND revoked_at IS NULL AND expires_at > now()
	`, tokenHash).Scan(&userID, &tenantSlug)
	return userID, tenantSlug, err
}
