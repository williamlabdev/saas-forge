package outbox

import (
	"context"
	"encoding/json"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// Row is a pending or processed outbox entry.
type Row struct {
	ID             uuid.UUID
	AggregateID    uuid.UUID
	EventType      string
	Payload        json.RawMessage
	Status         string
	RetryCount     int
	IdempotencyKey string
	LastError      *string
	CreatedAt      time.Time
}

// Repository persists integration outbox rows.
type Repository interface {
	EnqueueTx(ctx context.Context, tx pgx.Tx, aggregateID uuid.UUID, eventType string, payload json.RawMessage, idempotencyKey string) error
	ClaimPending(ctx context.Context, limit int) ([]Row, error)
	MarkDone(ctx context.Context, id uuid.UUID) error
	MarkFailedWithRetry(ctx context.Context, id uuid.UUID, lastErr string, maxRetries int) (dead bool, err error)
	// ReclaimStale resets rows stuck in 'processing' beyond olderThan back to
	// 'pending' (crash/shutdown safety net) and returns how many were reclaimed.
	ReclaimStale(ctx context.Context, olderThan time.Duration) (int64, error)
	CountPending(ctx context.Context) (int64, error)
	// PendingStats returns pending/processing counts and their lag seconds; lag is 0 when empty.
	PendingStats(ctx context.Context) (PendingStats, error)
}

// PendingStats is used for outbox lag gauges.
type PendingStats struct {
	Pending              int64
	LagSeconds           float64
	Processing           int64
	ProcessingLagSeconds float64
}
