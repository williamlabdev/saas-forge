package repository

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	apperrors "github.com/williamlabdev/saas-forge/internal/pkg/errors"
	"github.com/williamlabdev/saas-forge/internal/pkg/outbox"
	"github.com/williamlabdev/saas-forge/internal/ticket/domain"
)

type PostgresTicketRepository struct {
	pool   *pgxpool.Pool
	outbox outbox.Repository
}

func NewPostgresTicketRepository(pool *pgxpool.Pool, ob outbox.Repository) *PostgresTicketRepository {
	return &PostgresTicketRepository{pool: pool, outbox: ob}
}

// Create inserts the row and, in the SAME transaction, enqueues an integration
// outbox event so the write and the event are atomic (transactional outbox).
func (r *PostgresTicketRepository) Create(ctx context.Context, m *domain.Ticket) error {
	if m.Version == 0 {
		m.Version = 1
	}
	if r.outbox == nil {
		_, err := r.pool.Exec(ctx, `
			INSERT INTO tickets (id, owner_id, name, status, version, created_at, updated_at)
			VALUES ($1, $2, $3, $4, $5, $6, $7)`,
			m.ID, m.OwnerID, m.Name, m.Status, m.Version, m.CreatedAt, m.UpdatedAt,
		)
		return err
	}

	tx, err := r.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()

	if _, err := tx.Exec(ctx, `
		INSERT INTO tickets (id, owner_id, name, status, version, created_at, updated_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7)`,
		m.ID, m.OwnerID, m.Name, m.Status, m.Version, m.CreatedAt, m.UpdatedAt,
	); err != nil {
		return err
	}

	if err := enqueueTicketEvent(ctx, tx, r.outbox, m, domain.EventTicketCreated, m.Version); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

func (r *PostgresTicketRepository) GetByID(ctx context.Context, id uuid.UUID) (*domain.Ticket, error) {
	row := r.pool.QueryRow(ctx, `
		SELECT id, owner_id, name, status, version, created_at, updated_at
		FROM tickets
		WHERE id = $1`, id)
	var m domain.Ticket
	if err := row.Scan(&m.ID, &m.OwnerID, &m.Name, &m.Status, &m.Version, &m.CreatedAt, &m.UpdatedAt); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, apperrors.ErrNotFound
		}
		return nil, fmt.Errorf("ticket scan: %w", err)
	}
	return &m, nil
}

func (r *PostgresTicketRepository) List(ctx context.Context, f ListFilter) (ListResult, error) {
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
		`SELECT COUNT(*) FROM tickets WHERE owner_id = $1`, f.OwnerID,
	).Scan(&total); err != nil {
		return ListResult{}, fmt.Errorf("tickets count: %w", err)
	}

	rows, err := r.pool.Query(ctx, `
		SELECT id, owner_id, name, status, version, created_at, updated_at
		FROM tickets
		WHERE owner_id = $1
		ORDER BY created_at DESC
		LIMIT $2 OFFSET $3`, f.OwnerID, limit, offset)
	if err != nil {
		return ListResult{}, fmt.Errorf("tickets list: %w", err)
	}
	defer rows.Close()

	var items []*domain.Ticket
	for rows.Next() {
		var m domain.Ticket
		if err := rows.Scan(&m.ID, &m.OwnerID, &m.Name, &m.Status, &m.Version, &m.CreatedAt, &m.UpdatedAt); err != nil {
			return ListResult{}, fmt.Errorf("tickets scan: %w", err)
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
func (r *PostgresTicketRepository) Update(ctx context.Context, m *domain.Ticket) error {
	if r.outbox == nil {
		tag, err := r.pool.Exec(ctx, `
			UPDATE tickets
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
	defer func() { _ = tx.Rollback(ctx) }()

	if err := tx.QueryRow(ctx, `
		UPDATE tickets
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

	if err := enqueueTicketEvent(ctx, tx, r.outbox, m, domain.EventTicketUpdated, m.Version); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

// Delete removes the row and, in the SAME transaction, enqueues a "deleted"
// integration event keyed by version+1 so it never collides with prior events.
func (r *PostgresTicketRepository) Delete(ctx context.Context, id uuid.UUID) error {
	if r.outbox == nil {
		tag, err := r.pool.Exec(ctx, `DELETE FROM tickets WHERE id = $1`, id)
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
	defer func() { _ = tx.Rollback(ctx) }()

	m := &domain.Ticket{ID: id}
	if err := tx.QueryRow(ctx, `
		DELETE FROM tickets
		WHERE id = $1
		RETURNING owner_id, name, status, version`, id,
	).Scan(&m.OwnerID, &m.Name, &m.Status, &m.Version); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return apperrors.ErrNotFound
		}
		return err
	}

	if err := enqueueTicketEvent(ctx, tx, r.outbox, m, domain.EventTicketDeleted, m.Version+1); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

// enqueueTicketEvent writes an integration_outbox row inside the given tx.
// version makes the idempotency key unique per state change.
func enqueueTicketEvent(ctx context.Context, tx pgx.Tx, ob outbox.Repository, m *domain.Ticket, eventType string, version int) error {
	payload, err := json.Marshal(ticketPayload{
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

type ticketPayload struct {
	ID      string `json:"id"`
	OwnerID string `json:"owner_id"`
	Name    string `json:"name"`
	Status  string `json:"status"`
}
