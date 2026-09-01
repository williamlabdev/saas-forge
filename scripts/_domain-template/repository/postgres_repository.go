package repository

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"__MODULE__/internal/__domain__/domain"
	apperrors "__MODULE__/internal/pkg/errors"
	"__MODULE__/internal/pkg/outbox"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

type Postgres__Domain__Repository struct {
	pool   *pgxpool.Pool
	outbox outbox.Repository
}

func NewPostgres__Domain__Repository(pool *pgxpool.Pool, ob outbox.Repository) *Postgres__Domain__Repository {
	return &Postgres__Domain__Repository{pool: pool, outbox: ob}
}

// Create inserts the row and, in the SAME transaction, enqueues an integration
// outbox event so the write and the event are atomic (transactional outbox).
func (r *Postgres__Domain__Repository) Create(ctx context.Context, m *domain.__Domain__) error {
	if m.Version == 0 {
		m.Version = 1
	}
	if r.outbox == nil {
		_, err := r.pool.Exec(ctx, `
			INSERT INTO __domains__ (id, owner_id, name, status, version, created_at, updated_at)
			VALUES ($1, $2, $3, $4, $5, $6, $7)`,
			m.ID, m.OwnerID, m.Name, m.Status, m.Version, m.CreatedAt, m.UpdatedAt,
		)
		return err
	}

	tx, err := r.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)

	if _, err := tx.Exec(ctx, `
		INSERT INTO __domains__ (id, owner_id, name, status, version, created_at, updated_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7)`,
		m.ID, m.OwnerID, m.Name, m.Status, m.Version, m.CreatedAt, m.UpdatedAt,
	); err != nil {
		return err
	}

	if err := enqueue__Domain__Event(ctx, tx, r.outbox, m, domain.Event__Domain__Created, m.Version); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

func (r *Postgres__Domain__Repository) GetByID(ctx context.Context, id uuid.UUID) (*domain.__Domain__, error) {
	row := r.pool.QueryRow(ctx, `
		SELECT id, owner_id, name, status, version, created_at, updated_at
		FROM __domains__
		WHERE id = $1`, id)
	var m domain.__Domain__
	if err := row.Scan(&m.ID, &m.OwnerID, &m.Name, &m.Status, &m.Version, &m.CreatedAt, &m.UpdatedAt); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, apperrors.ErrNotFound
		}
		return nil, fmt.Errorf("__domain__ scan: %w", err)
	}
	return &m, nil
}

func (r *Postgres__Domain__Repository) List(ctx context.Context, f ListFilter) (ListResult, error) {
	limit := f.Limit
	if limit <= 0 || limit > 100 {
		limit = 20
	}
	offset := f.Offset
	if offset < 0 {
		offset = 0
	}

	var total int
	if err := r.pool.QueryRow(ctx,
		`SELECT COUNT(*) FROM __domains__ WHERE owner_id = $1`, f.OwnerID,
	).Scan(&total); err != nil {
		return ListResult{}, fmt.Errorf("__domains__ count: %w", err)
	}

	rows, err := r.pool.Query(ctx, `
		SELECT id, owner_id, name, status, version, created_at, updated_at
		FROM __domains__
		WHERE owner_id = $1
		ORDER BY created_at DESC
		LIMIT $2 OFFSET $3`, f.OwnerID, limit, offset)
	if err != nil {
		return ListResult{}, fmt.Errorf("__domains__ list: %w", err)
	}
	defer rows.Close()

	var items []*domain.__Domain__
	for rows.Next() {
		var m domain.__Domain__
		if err := rows.Scan(&m.ID, &m.OwnerID, &m.Name, &m.Status, &m.Version, &m.CreatedAt, &m.UpdatedAt); err != nil {
			return ListResult{}, fmt.Errorf("__domains__ scan: %w", err)
		}
		items = append(items, &m)
	}
	if err := rows.Err(); err != nil {
		return ListResult{}, err
	}
	return ListResult{Items: items, Total: total}, nil
}

// Update bumps the row's version and, in the SAME transaction, enqueues an
// "updated" integration event keyed by the new version (idempotent + atomic).
func (r *Postgres__Domain__Repository) Update(ctx context.Context, m *domain.__Domain__) error {
	if r.outbox == nil {
		tag, err := r.pool.Exec(ctx, `
			UPDATE __domains__
			SET name = $2, status = $3, updated_at = $4, version = version + 1
			WHERE id = $1`,
			m.ID, m.Name, m.Status, m.UpdatedAt,
		)
		if err != nil {
			return err
		}
		if tag.RowsAffected() == 0 {
			return apperrors.ErrNotFound
		}
		return nil
	}

	tx, err := r.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)

	if err := tx.QueryRow(ctx, `
		UPDATE __domains__
		SET name = $2, status = $3, updated_at = $4, version = version + 1
		WHERE id = $1
		RETURNING version`,
		m.ID, m.Name, m.Status, m.UpdatedAt,
	).Scan(&m.Version); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return apperrors.ErrNotFound
		}
		return err
	}

	if err := enqueue__Domain__Event(ctx, tx, r.outbox, m, domain.Event__Domain__Updated, m.Version); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

// Delete removes the row and, in the SAME transaction, enqueues a "deleted"
// integration event keyed by version+1 so it never collides with prior events.
func (r *Postgres__Domain__Repository) Delete(ctx context.Context, id uuid.UUID) error {
	if r.outbox == nil {
		tag, err := r.pool.Exec(ctx, `DELETE FROM __domains__ WHERE id = $1`, id)
		if err != nil {
			return err
		}
		if tag.RowsAffected() == 0 {
			return apperrors.ErrNotFound
		}
		return nil
	}

	tx, err := r.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)

	m := &domain.__Domain__{ID: id}
	if err := tx.QueryRow(ctx, `
		DELETE FROM __domains__
		WHERE id = $1
		RETURNING owner_id, name, status, version`, id,
	).Scan(&m.OwnerID, &m.Name, &m.Status, &m.Version); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return apperrors.ErrNotFound
		}
		return err
	}

	if err := enqueue__Domain__Event(ctx, tx, r.outbox, m, domain.Event__Domain__Deleted, m.Version+1); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

// enqueue__Domain__Event writes an integration_outbox row inside the given tx.
// version makes the idempotency key unique per state change.
func enqueue__Domain__Event(ctx context.Context, tx pgx.Tx, ob outbox.Repository, m *domain.__Domain__, eventType string, version int) error {
	payload, err := json.Marshal(__domain__Payload{
		ID:      m.ID.String(),
		OwnerID: m.OwnerID.String(),
		Name:    m.Name,
		Status:  m.Status,
	})
	if err != nil {
		return err
	}
	key := outbox.IdempotencyKey(m.ID, version)
	return ob.EnqueueTx(ctx, tx, m.ID, eventType, payload, key)
}

type __domain__Payload struct {
	ID      string `json:"id"`
	OwnerID string `json:"owner_id"`
	Name    string `json:"name"`
	Status  string `json:"status"`
}
