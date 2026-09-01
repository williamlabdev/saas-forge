package repository

import (
	"context"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/williamlabdev/saas-forge/internal/notification/domain"
)

type PostgresNotificationRepository struct {
	pool *pgxpool.Pool
}

func NewPostgresNotificationRepository(pool *pgxpool.Pool) *PostgresNotificationRepository {
	return &PostgresNotificationRepository{pool: pool}
}

func (r *PostgresNotificationRepository) Create(ctx context.Context, n *domain.Notification) error {
	_, err := r.pool.Exec(ctx, `
		INSERT INTO in_app_notifications (id, user_id, title, body, created_at)
		VALUES ($1, $2, $3, $4, $5)`,
		n.ID, n.UserID, n.Title, n.Body, n.CreatedAt,
	)
	return err
}

func (r *PostgresNotificationRepository) ListForUser(ctx context.Context, userID uuid.UUID, limit int) ([]*domain.Notification, error) {
	if limit <= 0 || limit > 100 {
		limit = 20
	}
	rows, err := r.pool.Query(ctx, `
		SELECT id, user_id, title, body, read_at, created_at
		FROM in_app_notifications
		WHERE user_id = $1
		ORDER BY created_at DESC
		LIMIT $2`, userID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*domain.Notification
	for rows.Next() {
		var n domain.Notification
		if err := rows.Scan(&n.ID, &n.UserID, &n.Title, &n.Body, &n.ReadAt, &n.CreatedAt); err != nil {
			return nil, fmt.Errorf("notification scan: %w", err)
		}
		out = append(out, &n)
	}
	return out, rows.Err()
}
