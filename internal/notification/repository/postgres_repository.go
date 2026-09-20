package repository

import (
	"context"
	"errors"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/williamlabdev/saas-forge/internal/notification/domain"
	apperrors "github.com/williamlabdev/saas-forge/internal/pkg/errors"
)

const defaultListLimit = 20
const maxListLimit = 100

type PostgresNotificationRepository struct {
	pool *pgxpool.Pool
}

func NewPostgresNotificationRepository(pool *pgxpool.Pool) *PostgresNotificationRepository {
	return &PostgresNotificationRepository{pool: pool}
}

func (r *PostgresNotificationRepository) Create(ctx context.Context, n *domain.Notification) error {
	_, err := r.pool.Exec(ctx, `
		INSERT INTO in_app_notifications (id, user_id, tenant_id, title, body, kind, link, created_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8)`,
		n.ID, n.UserID, n.TenantID, n.Title, n.Body, n.Kind, n.Link, n.CreatedAt,
	)
	return err
}

func clampLimit(limit int) int {
	if limit <= 0 || limit > maxListLimit {
		return defaultListLimit
	}
	return limit
}

func (r *PostgresNotificationRepository) ListForUser(ctx context.Context, userID uuid.UUID, filter ListFilter) ([]*domain.Notification, error) {
	limit := clampLimit(filter.Limit)
	// tenant_id IS NULL always matches: a nil-tenant row (self-notify, or a
	// future platform-wide notice) belongs to every tenant view — see
	// migration 000049's comment and domain.Notification.TenantID's doc.
	//
	// When filter.TenantID itself is nil (no active tenant on the caller's
	// subject), "tenant_id = NULL" is SQL UNKNOWN rather than true, so the OR
	// correctly collapses to "tenant_id IS NULL only" instead of matching
	// every tenant's rows — there is deliberately no third "no filter at all"
	// branch here, because that would leak another tenant's notices to a
	// caller who has none active.
	rows, err := r.pool.Query(ctx, `
		SELECT id, user_id, tenant_id, title, body, kind, link, read_at, created_at
		FROM in_app_notifications
		WHERE user_id = $1
		  AND (tenant_id = $2::uuid OR tenant_id IS NULL)
		  AND (NOT $3 OR read_at IS NULL)
		ORDER BY created_at DESC
		LIMIT $4`, userID, filter.TenantID, filter.UnreadOnly, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*domain.Notification
	for rows.Next() {
		n, err := scanNotification(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, n)
	}
	return out, rows.Err()
}

func (r *PostgresNotificationRepository) UnreadCount(ctx context.Context, userID uuid.UUID, tenantID *uuid.UUID) (int, error) {
	var count int
	err := r.pool.QueryRow(ctx, `
		SELECT count(*)
		FROM in_app_notifications
		WHERE user_id = $1
		  AND (tenant_id = $2::uuid OR tenant_id IS NULL)
		  AND read_at IS NULL`, userID, tenantID).Scan(&count)
	if err != nil {
		return 0, err
	}
	return count, nil
}

// MarkRead is idempotent (UPDATE ... WHERE read_at IS NULL would make a
// second call on an already-read row match zero rows and misreport as "not
// found"), so the WHERE clause only scopes ownership — read_at is set
// unconditionally to now for a row that already belongs to userID, and an
// already-read row is simply overwritten with a fresh read_at rather than
// left untouched. That trade (a read timestamp can move forward on a repeat
// call) is preferable to a mark-read endpoint whose second call 404s on a
// notification the caller can plainly see in their own list.
func (r *PostgresNotificationRepository) MarkRead(ctx context.Context, userID, id uuid.UUID) (*domain.Notification, error) {
	row := r.pool.QueryRow(ctx, `
		UPDATE in_app_notifications
		SET read_at = COALESCE(read_at, NOW())
		WHERE id = $1 AND user_id = $2
		RETURNING id, user_id, tenant_id, title, body, kind, link, read_at, created_at`,
		id, userID)
	n, err := scanNotification(row)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			// Deliberately the SAME error whether id does not exist at all or
			// belongs to someone else — existence of another user's row is not
			// observable from this endpoint's response (task requirement 2).
			return nil, apperrors.ErrNotFound
		}
		return nil, err
	}
	return n, nil
}

func (r *PostgresNotificationRepository) MarkAllRead(ctx context.Context, userID uuid.UUID, tenantID *uuid.UUID) (int, error) {
	tag, err := r.pool.Exec(ctx, `
		UPDATE in_app_notifications
		SET read_at = NOW()
		WHERE user_id = $1
		  AND (tenant_id = $2::uuid OR tenant_id IS NULL)
		  AND read_at IS NULL`, userID, tenantID)
	if err != nil {
		return 0, err
	}
	return int(tag.RowsAffected()), nil
}

// rowScanner is satisfied by both pgx.Rows (ListForUser) and pgx.Row
// (MarkRead's RETURNING), so the same column list is decoded in one place.
type rowScanner interface {
	Scan(dest ...any) error
}

func scanNotification(row rowScanner) (*domain.Notification, error) {
	var n domain.Notification
	if err := row.Scan(&n.ID, &n.UserID, &n.TenantID, &n.Title, &n.Body, &n.Kind, &n.Link, &n.ReadAt, &n.CreatedAt); err != nil {
		return nil, fmt.Errorf("notification scan: %w", err)
	}
	return &n, nil
}
