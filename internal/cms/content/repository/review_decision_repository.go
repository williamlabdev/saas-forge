package repository

import (
	"context"
	"errors"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/williamlabdev/saas-forge/internal/cms/content/domain"
)

// --- review decisions (ADR-014 Amendment: review decisions) -----------------

// reviewDecisionColumns is shared by every read here, for the reason
// scheduleColumns gives: a column written but not read back comes back as its
// zero value, and for `entry_version` that would break every caller comparing
// it against the entry's current version.
const reviewDecisionColumns = `id, tenant_id, entry_id, decision, reason,
	entry_version, decided_by, decided_at`

func scanReviewDecision(row pgx.Row) (*domain.EntryReviewDecision, error) {
	var d domain.EntryReviewDecision
	if err := row.Scan(
		&d.ID, &d.TenantID, &d.EntryID, &d.Decision, &d.Reason,
		&d.EntryVersion, &d.DecidedBy, &d.DecidedAt,
	); err != nil {
		return nil, err
	}
	return &d, nil
}

// CreateEntryReviewDecision inserts one decision. There is no unique index to
// violate — unlike a schedule, an entry may collect any number of decisions
// over its life, one per time a reviewer looked and sent it back.
func (r *PostgresContentRepository) CreateEntryReviewDecision(ctx context.Context, d *domain.EntryReviewDecision) error {
	return r.withTenant(ctx, d.TenantID, func(q querier) error {
		_, err := q.Exec(ctx, `
			INSERT INTO entry_review_decisions
				(id, tenant_id, entry_id, decision, reason, entry_version, decided_by, decided_at)
			VALUES ($1, $2, $3, $4, $5, $6, $7, $8)`,
			d.ID, d.TenantID, d.EntryID, d.Decision, d.Reason,
			d.EntryVersion, d.DecidedBy, d.DecidedAt,
		)
		if err != nil {
			return fmt.Errorf("insert entry review decision: %w", err)
		}
		return nil
	})
}

// GetLatestEntryReviewDecision returns the newest decision for the entry, or
// (nil, nil) when it has never been sent back.
//
// nil-nil rather than ErrNotFound for GetPendingEntrySchedule's reason: "no
// decision" is the ordinary state of almost every entry, and both callers
// (dto.go's decoration, the wasPending fix in UpdateEntry) ask this on entries
// they expect to answer no for.
func (r *PostgresContentRepository) GetLatestEntryReviewDecision(ctx context.Context, tenantID string, entryID uuid.UUID) (*domain.EntryReviewDecision, error) {
	var out *domain.EntryReviewDecision
	err := r.withTenant(ctx, tenantID, func(q querier) error {
		d, err := scanReviewDecision(q.QueryRow(ctx, `
			SELECT `+reviewDecisionColumns+`
			FROM entry_review_decisions
			WHERE tenant_id = $1 AND entry_id = $2
			ORDER BY decided_at DESC, id DESC
			LIMIT 1`, tenantID, entryID))
		if err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return nil
			}
			return err
		}
		out = d
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("get latest entry review decision: %w", err)
	}
	return out, nil
}

// ListLatestEntryReviewDecisions is GetLatestEntryReviewDecision for a page of
// entries at once, the same DISTINCT ON shape ListActionableEntrySchedules
// uses. Entries with no decision are simply absent from the map.
func (r *PostgresContentRepository) ListLatestEntryReviewDecisions(ctx context.Context, tenantID string, entryIDs []uuid.UUID) (map[uuid.UUID]*domain.EntryReviewDecision, error) {
	if len(entryIDs) == 0 {
		return nil, nil
	}
	out := make(map[uuid.UUID]*domain.EntryReviewDecision, len(entryIDs))
	err := r.withTenant(ctx, tenantID, func(q querier) error {
		rows, err := q.Query(ctx, `
			SELECT DISTINCT ON (entry_id) `+reviewDecisionColumns+`
			FROM entry_review_decisions
			WHERE tenant_id = $1 AND entry_id = ANY($2)
			ORDER BY entry_id, decided_at DESC, id DESC`,
			tenantID, entryIDs)
		if err != nil {
			return fmt.Errorf("list latest entry review decisions: %w", err)
		}
		defer rows.Close()
		for rows.Next() {
			d, err := scanReviewDecision(rows)
			if err != nil {
				return fmt.Errorf("list latest entry review decisions scan: %w", err)
			}
			out[d.EntryID] = d
		}
		return rows.Err()
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// ListEntryReviewDecisions returns the entry's full decision history, newest
// first, capped at domain.MaxReviewDecisionsPerEntry.
func (r *PostgresContentRepository) ListEntryReviewDecisions(ctx context.Context, tenantID string, entryID uuid.UUID) ([]domain.EntryReviewDecision, error) {
	var out []domain.EntryReviewDecision
	err := r.withTenant(ctx, tenantID, func(q querier) error {
		rows, err := q.Query(ctx, `
			SELECT `+reviewDecisionColumns+`
			FROM entry_review_decisions
			WHERE tenant_id = $1 AND entry_id = $2
			ORDER BY decided_at DESC, id DESC
			LIMIT $3`, tenantID, entryID, domain.MaxReviewDecisionsPerEntry)
		if err != nil {
			return fmt.Errorf("list entry review decisions: %w", err)
		}
		defer rows.Close()
		for rows.Next() {
			d, err := scanReviewDecision(rows)
			if err != nil {
				return fmt.Errorf("list entry review decisions scan: %w", err)
			}
			out = append(out, *d)
		}
		return rows.Err()
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}
