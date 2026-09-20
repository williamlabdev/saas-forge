package repository

import (
	"context"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/williamlabdev/saas-forge/internal/auth/audit"
)

// PostgresAuditRepository appends rows to auth_audit_events.
type PostgresAuditRepository struct {
	pool *pgxpool.Pool
}

func NewPostgresAuditRepository(pool *pgxpool.Pool) *PostgresAuditRepository {
	return &PostgresAuditRepository{pool: pool}
}

func (r *PostgresAuditRepository) Record(ctx context.Context, e audit.Entry) error {
	var userID any
	if e.UserID != nil {
		userID = *e.UserID
	}
	var errCode any
	if e.ErrorCode != "" {
		errCode = e.ErrorCode
	}
	_, err := r.pool.Exec(ctx, `
		INSERT INTO auth_audit_events (event_type, outcome, user_id, client_ip, user_agent, error_code)
		VALUES ($1, $2, $3, $4, $5, $6)
	`, string(e.EventType), string(e.Outcome), userID, nullIfEmpty(e.ClientIP), nullIfEmpty(e.UserAgent), errCode)
	return err
}

func nullIfEmpty(s string) any {
	if s == "" {
		return nil
	}
	return s
}
