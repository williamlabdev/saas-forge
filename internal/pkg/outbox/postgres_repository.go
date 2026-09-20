package outbox

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// PostgresRepository implements Repository against integration_outbox.
type PostgresRepository struct {
	pool *pgxpool.Pool
}

func NewPostgresRepository(pool *pgxpool.Pool) *PostgresRepository {
	return &PostgresRepository{pool: pool}
}

func (r *PostgresRepository) EnqueueTx(ctx context.Context, tx pgx.Tx, aggregateID uuid.UUID, eventType string, payload json.RawMessage, idempotencyKey string) error {
	_, err := tx.Exec(ctx, `
		INSERT INTO integration_outbox (aggregate_id, event_type, payload, idempotency_key)
		VALUES ($1, $2, $3, $4)
		ON CONFLICT (idempotency_key) DO NOTHING
	`, aggregateID, eventType, payload, idempotencyKey)
	return err
}

func (r *PostgresRepository) ClaimPending(ctx context.Context, limit int) ([]Row, error) {
	rows, err := r.pool.Query(ctx, `
		UPDATE integration_outbox
		SET status = 'processing', claimed_at = now()
		WHERE id IN (
			SELECT id FROM integration_outbox
			WHERE status = 'pending'
			ORDER BY created_at
			LIMIT $1
			FOR UPDATE SKIP LOCKED
		)
		RETURNING id, aggregate_id, event_type, payload, status, retry_count, idempotency_key, last_error, created_at
	`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []Row
	for rows.Next() {
		var row Row
		var lastErr *string
		if err := rows.Scan(
			&row.ID, &row.AggregateID, &row.EventType, &row.Payload,
			&row.Status, &row.RetryCount, &row.IdempotencyKey, &lastErr, &row.CreatedAt,
		); err != nil {
			return nil, err
		}
		row.LastError = lastErr
		out = append(out, row)
	}
	return out, rows.Err()
}

func (r *PostgresRepository) MarkDone(ctx context.Context, id uuid.UUID) error {
	_, err := r.pool.Exec(ctx, `
		UPDATE integration_outbox
		SET status = 'done', processed_at = now(), last_error = NULL
		WHERE id = $1
	`, id)
	return err
}

func (r *PostgresRepository) MarkFailedWithRetry(ctx context.Context, id uuid.UUID, lastErr string, maxRetries int) (bool, error) {
	var dead bool
	err := r.pool.QueryRow(ctx, `
		UPDATE integration_outbox
		SET
			retry_count = retry_count + 1,
			last_error = $2,
			status = CASE WHEN retry_count + 1 >= $3 THEN 'failed'::outbox_status ELSE 'pending'::outbox_status END
		WHERE id = $1
		RETURNING (status = 'failed'::outbox_status)
	`, id, lastErr, maxRetries).Scan(&dead)
	return dead, err
}

// ReclaimStale resets rows stuck in 'processing' for longer than olderThan back
// to 'pending' so they are redelivered. This is the crash/shutdown safety net:
// it only touches rows that were claimed but never marked done or failed, so it
// deliberately does NOT increment retry_count — a row interrupted by a deploy is
// healthy, not poison. Rows whose delivery actually failed are handled by
// MarkFailedWithRetry instead. Returns the number of rows reclaimed.
func (r *PostgresRepository) ReclaimStale(ctx context.Context, olderThan time.Duration) (int64, error) {
	tag, err := r.pool.Exec(ctx, `
		UPDATE integration_outbox
		SET status = 'pending', claimed_at = NULL, last_error = 'reclaimed: stale processing'
		WHERE status = 'processing'
			AND claimed_at IS NOT NULL
			AND claimed_at < now() - make_interval(secs => $1)
	`, olderThan.Seconds())
	if err != nil {
		return 0, err
	}
	return tag.RowsAffected(), nil
}

func (r *PostgresRepository) CountPending(ctx context.Context) (int64, error) {
	st, err := r.PendingStats(ctx)
	return st.Pending, err
}

func (r *PostgresRepository) PendingStats(ctx context.Context) (PendingStats, error) {
	var st PendingStats
	err := r.pool.QueryRow(ctx, `
		SELECT
			COUNT(*) FILTER (WHERE status = 'pending')::bigint,
			COALESCE(EXTRACT(EPOCH FROM (now() - MIN(created_at) FILTER (WHERE status = 'pending'))), 0),
			COUNT(*) FILTER (WHERE status = 'processing')::bigint,
			COALESCE(EXTRACT(EPOCH FROM (now() - MIN(claimed_at) FILTER (WHERE status = 'processing'))), 0)
		FROM integration_outbox
		WHERE status IN ('pending', 'processing')
	`).Scan(&st.Pending, &st.LagSeconds, &st.Processing, &st.ProcessingLagSeconds)
	return st, err
}

// ParseUserPayload decodes a user.* event payload.
func ParseUserPayload(raw json.RawMessage) (UserPayload, error) {
	var p UserPayload
	if err := json.Unmarshal(raw, &p); err != nil {
		return UserPayload{}, fmt.Errorf("outbox: parse payload: %w", err)
	}
	return p, nil
}
