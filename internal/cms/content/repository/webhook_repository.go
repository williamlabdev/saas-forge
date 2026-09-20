package repository

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/williamlabdev/saas-forge/internal/cms/content/domain"
	apperrors "github.com/williamlabdev/saas-forge/internal/pkg/errors"
	"github.com/williamlabdev/saas-forge/internal/pkg/outbox"
)

// --- webhook registry (ADR-011) ----------------------------------------------

func (r *PostgresContentRepository) CreateWebhook(ctx context.Context, w *domain.Webhook) error {
	return r.withTenant(ctx, w.TenantID, func(q querier) error {
		_, err := q.Exec(ctx, `
			INSERT INTO content_webhooks (id, tenant_id, url, secret, active, description, created_at, updated_at)
			VALUES ($1, $2, $3, $4, $5, $6, $7, $7)`,
			w.ID, w.TenantID, w.URL, w.Secret, w.Active, w.Description, w.CreatedAt,
		)
		if err != nil {
			return fmt.Errorf("insert webhook: %w", err)
		}
		return nil
	})
}

func (r *PostgresContentRepository) ListWebhooks(ctx context.Context, tenantID string) ([]*domain.Webhook, error) {
	var out []*domain.Webhook
	err := r.withTenant(ctx, tenantID, func(q querier) error {
		rows, err := q.Query(ctx, `
			SELECT id, tenant_id, url, secret, active, description, created_at, updated_at
			FROM content_webhooks WHERE tenant_id = $1 ORDER BY created_at, id`, tenantID)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var w domain.Webhook
			if err := rows.Scan(&w.ID, &w.TenantID, &w.URL, &w.Secret, &w.Active, &w.Description, &w.CreatedAt, &w.UpdatedAt); err != nil {
				return err
			}
			out = append(out, &w)
		}
		return rows.Err()
	})
	if err != nil {
		return nil, fmt.Errorf("list webhooks: %w", err)
	}
	return out, nil
}

func (r *PostgresContentRepository) DeleteWebhook(ctx context.Context, tenantID string, id uuid.UUID) error {
	var affected int64
	if err := r.withTenant(ctx, tenantID, func(q querier) error {
		tag, err := q.Exec(ctx,
			`DELETE FROM content_webhooks WHERE tenant_id = $1 AND id = $2`, tenantID, id)
		if err != nil {
			return fmt.Errorf("delete webhook: %w", err)
		}
		affected = tag.RowsAffected()
		return nil
	}); err != nil {
		return err
	}
	if affected == 0 {
		return apperrors.ErrNotFound
	}
	return nil
}

// ActiveWebhookEndpoints implements outbox.WebhookDirectory: the worker's
// delivery-time question, answered tenant-scoped so RLS applies to it exactly
// as to the registry CRUD.
func (r *PostgresContentRepository) ActiveWebhookEndpoints(ctx context.Context, tenantID string) ([]outbox.WebhookEndpoint, error) {
	var out []outbox.WebhookEndpoint
	err := r.withTenant(ctx, tenantID, func(q querier) error {
		rows, err := q.Query(ctx, `
			SELECT id, url, secret FROM content_webhooks
			WHERE tenant_id = $1 AND active ORDER BY created_at, id`, tenantID)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var ep outbox.WebhookEndpoint
			if err := rows.Scan(&ep.ID, &ep.URL, &ep.Secret); err != nil {
				return err
			}
			out = append(out, ep)
		}
		return rows.Err()
	})
	if err != nil {
		return nil, fmt.Errorf("active webhook endpoints: %w", err)
	}
	return out, nil
}

// --- event emission -----------------------------------------------------------

// emitEntryEvent enqueues one content event IN THE CALLER'S TRANSACTION — the
// transactional-outbox pattern the handle was retained for. The write and its
// event commit or roll back together; there is no state where one exists
// without the other.
//
// The type NAME is resolved here, in the same transaction, because consumers
// think in names and the entry row only carries the id. On the delete path the
// type still exists (deleting a TYPE refuses while entries remain), so the
// lookup cannot miss.
//
// A nil outbox skips emission: that is the composition root saying this
// deployment has no integration plane (unit fixtures, one-off tools). The
// worker's fail-loud default arm covers the opposite mistake — events emitted
// with nobody wired to deliver them.
func (r *PostgresContentRepository) emitEntryEvent(ctx context.Context, q querier, eventType, tenantID string, contentTypeID, entryID uuid.UUID, locale, idemKey string) error {
	if r.outbox == nil {
		return nil
	}
	tx, ok := q.(pgx.Tx)
	if !ok {
		// Structurally unreachable — withTenant and WithTx both hand out pgx.Tx —
		// but an event quietly written OUTSIDE the transaction would survive its
		// rollback, which is the exact corruption outbox exists to prevent.
		return fmt.Errorf("content: emit %s outside a transaction", eventType)
	}
	var typeName string
	if err := q.QueryRow(ctx,
		`SELECT name FROM content_types WHERE tenant_id = $1 AND id = $2`,
		tenantID, contentTypeID,
	).Scan(&typeName); err != nil {
		return fmt.Errorf("emit %s: resolve type name: %w", eventType, err)
	}
	payload, err := json.Marshal(outbox.ContentEventPayload{
		TenantID:    tenantID,
		EntryID:     entryID.String(),
		ContentType: typeName,
		Locale:      locale,
	})
	if err != nil {
		return err
	}
	return r.outbox.EnqueueTx(ctx, tx, entryID, eventType, payload, idemKey)
}
