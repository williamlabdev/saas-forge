package repository

import (
	"context"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/williamlabdev/saas-forge/internal/cms/content/domain"
)

// --- media transforms (ADR-019) ----------------------------------------------

// MediaVariantAssetRef is one unit of worker work: an asset with at least one
// due pending variant. The worker claims the asset's rows together so the
// source is fetched and decoded once per asset, not once per preset.
type MediaVariantAssetRef struct {
	AssetID  uuid.UUID
	TenantID string
}

// MediaVariantOutcome is what the worker learned about one claimed row. Which
// fields matter depends on State:
//
//	done     — StorageKey, ContentType, SizeBytes, WidthPx, HeightPx
//	skipped  — SkipReason
//	pending  — Error, NextAttemptAt (a retry: attempts+1, still pending)
//	failed   — Error (terminal: attempts+1)
//
// One struct rather than four methods because the worker's decision is a
// single switch and the repository's job is one UPDATE; splitting it would put
// the same "which state am I in" logic on both sides of the interface.
type MediaVariantOutcome struct {
	State         string
	StorageKey    string
	ContentType   string
	SizeBytes     int64
	WidthPx       int
	HeightPx      int
	SkipReason    string
	Error         string
	NextAttemptAt *time.Time
}

const (
	mediaVariantBatchDefault = 10
	mediaVariantBatchMax     = 100
)

// mediaVariantColumns: same rule as scheduleColumns — one select list, so a
// column written but not read back cannot silently come back as its zero
// value. `state` reading as "" would make every row look non-pending and the
// worker would never pick anything up.
const mediaVariantColumns = `asset_id, tenant_id, preset, state,
	COALESCE(storage_key, ''), COALESCE(content_type, ''), COALESCE(size_bytes, 0),
	COALESCE(width_px, 0), COALESCE(height_px, 0),
	attempts, COALESCE(next_attempt_at, created_at), COALESCE(error, ''), COALESCE(skip_reason, ''),
	created_at, updated_at`

// mediaVariantOrder lists `original` first and then the presets by ascending
// width, so the DTO and the worker see the same stable order. The table has no
// width column to sort by; the CASE is the domain table spelled in SQL.
const mediaVariantOrder = `ORDER BY CASE preset
	WHEN 'original' THEN 0 WHEN 'thumb' THEN 1 WHEN 'small' THEN 2
	WHEN 'medium' THEN 3 WHEN 'large' THEN 4 ELSE 5 END`

func scanMediaVariant(row pgx.Row) (*domain.MediaVariant, error) {
	var v domain.MediaVariant
	if err := row.Scan(
		&v.AssetID, &v.TenantID, &v.Preset, &v.State,
		&v.StorageKey, &v.ContentType, &v.SizeBytes,
		&v.WidthPx, &v.HeightPx,
		&v.Attempts, &v.NextAttemptAt, &v.Error, &v.SkipReason,
		&v.CreatedAt, &v.UpdatedAt,
	); err != nil {
		return nil, err
	}
	return &v, nil
}

// EnqueueMediaVariants makes every preset row (plus `original`) pending for
// one asset. It is an upsert on purpose: the first call, from
// CompleteMediaUpload, inserts; a later call from POST /media/{id}/variants
// resets whatever is there. The reset keeps storage_key so a regenerated
// variant overwrites its previous object rather than leaking one, and clears
// everything else so the row reads as never having been tried.
func (r *PostgresContentRepository) EnqueueMediaVariants(ctx context.Context, tenantID string, assetID uuid.UUID, storageKey string) error {
	return r.withTenant(ctx, tenantID, func(q querier) error {
		presets := append([]string{domain.MediaPresetOriginal}, domain.MediaPresetNames()...)
		for _, p := range presets {
			var key *string
			if p == domain.MediaPresetOriginal {
				key = &storageKey
			}
			if _, err := q.Exec(ctx, `
				INSERT INTO media_variants (asset_id, tenant_id, preset, state, storage_key)
				VALUES ($1, $2, $3, $4, $5)
				ON CONFLICT (asset_id, preset) DO UPDATE SET
					state = EXCLUDED.state,
					storage_key = COALESCE(media_variants.storage_key, EXCLUDED.storage_key),
					attempts = 0, next_attempt_at = NULL, error = NULL, skip_reason = NULL,
					updated_at = NOW()`,
				assetID, tenantID, p, domain.MediaVariantPending, key); err != nil {
				return fmt.Errorf("enqueue media variant %s: %w", p, err)
			}
		}
		return nil
	})
}

// ListMediaVariants returns one asset's rows in display order. An asset with
// no rows (non-image, or uploaded before ADR-019) yields an empty slice, not
// an error — "no variants" is an ordinary answer.
func (r *PostgresContentRepository) ListMediaVariants(ctx context.Context, tenantID string, assetID uuid.UUID) ([]*domain.MediaVariant, error) {
	var out []*domain.MediaVariant
	err := r.withTenant(ctx, tenantID, func(q querier) error {
		rows, err := q.Query(ctx, `
			SELECT `+mediaVariantColumns+`
			FROM media_variants
			WHERE tenant_id = $1 AND asset_id = $2
			`+mediaVariantOrder, tenantID, assetID)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			v, err := scanMediaVariant(rows)
			if err != nil {
				return err
			}
			out = append(out, v)
		}
		return rows.Err()
	})
	if err != nil {
		return nil, fmt.Errorf("list media variants: %w", err)
	}
	return out, nil
}

// ListPendingMediaVariantAssets is the worker's CROSS-TENANT candidate scan,
// built the way ListDueEntrySchedules is and for the same reasons: its own
// short transaction, a transaction-local scan GUC that the SELECT-only policy
// media_variants_worker_scan reads, and a rollback so the pooled connection
// carries nothing forward. It locks nothing; ClaimPendingMediaVariants settles
// which replica gets the asset.
func (r *PostgresContentRepository) ListPendingMediaVariantAssets(ctx context.Context, now time.Time, limit int) ([]MediaVariantAssetRef, error) {
	if limit <= 0 {
		limit = mediaVariantBatchDefault
	}
	if limit > mediaVariantBatchMax {
		limit = mediaVariantBatchMax
	}
	if r.tx != nil {
		return nil, fmt.Errorf("content: ListPendingMediaVariantAssets is a cross-tenant scan and must not join a tenant-bound transaction")
	}
	tx, err := r.pool.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if _, err := tx.Exec(ctx, `SELECT set_config('app.media_transform_scan', 'on', true)`); err != nil {
		return nil, fmt.Errorf("set media transform scan context: %w", err)
	}
	// Grouped per asset: the unit of work is the asset, and the oldest due row
	// decides its place in the queue so a retry does not jump ahead of fresh
	// uploads. NULL next_attempt_at means "due now" (migration 000043).
	rows, err := tx.Query(ctx, `
		SELECT asset_id, tenant_id
		FROM media_variants
		WHERE state = $1 AND (next_attempt_at IS NULL OR next_attempt_at <= $2)
		GROUP BY asset_id, tenant_id
		ORDER BY MIN(COALESCE(next_attempt_at, created_at))
		LIMIT $3`, domain.MediaVariantPending, now, limit)
	if err != nil {
		return nil, fmt.Errorf("list pending media variant assets: %w", err)
	}
	defer rows.Close()
	var out []MediaVariantAssetRef
	for rows.Next() {
		var ref MediaVariantAssetRef
		if err := rows.Scan(&ref.AssetID, &ref.TenantID); err != nil {
			return nil, err
		}
		out = append(out, ref)
	}
	return out, rows.Err()
}

// ClaimPendingMediaVariants takes the row locks that ARE the claim on one
// asset's due rows, and must run inside WithTx by the caller that finishes
// them there. An empty result means another replica has the asset, the rows
// were finished meanwhile, or the asset was deleted (FK cascade) — all three
// mean "nothing to do here".
//
// Locking the variant rows also holds off a concurrent DeleteMediaAsset: its
// FK cascade waits for these locks, so the worker's UPDATE sees the rows and
// the delete then sees the keys the worker wrote (DeleteMediaAsset lists them
// FOR UPDATE for exactly this reason).
func (r *PostgresContentRepository) ClaimPendingMediaVariants(ctx context.Context, tenantID string, assetID uuid.UUID, now time.Time) ([]*domain.MediaVariant, error) {
	if r.tx == nil {
		return nil, fmt.Errorf("content: ClaimPendingMediaVariants must run inside WithTx — the row locks it takes are the claim, and a transaction that ends here releases them")
	}
	var out []*domain.MediaVariant
	err := r.withTenant(ctx, tenantID, func(q querier) error {
		rows, err := q.Query(ctx, `
			SELECT `+mediaVariantColumns+`
			FROM media_variants
			WHERE tenant_id = $1 AND asset_id = $2 AND state = $3
			  AND (next_attempt_at IS NULL OR next_attempt_at <= $4)
			`+mediaVariantOrder+`
			FOR UPDATE SKIP LOCKED`,
			tenantID, assetID, domain.MediaVariantPending, now)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			v, err := scanMediaVariant(rows)
			if err != nil {
				return err
			}
			out = append(out, v)
		}
		return rows.Err()
	})
	if err != nil {
		return nil, fmt.Errorf("claim pending media variants: %w", err)
	}
	return out, nil
}

// FinishMediaVariant records one claimed row's outcome. It returns false when
// the UPDATE matched nothing, which after a successful claim can only mean the
// asset was deleted underneath the worker — the caller then removes the object
// it just wrote. Retries (State pending) and terminal failures both bump
// attempts; done and skipped leave it alone so the count reads as "how many
// times did this go wrong".
func (r *PostgresContentRepository) FinishMediaVariant(ctx context.Context, tenantID string, assetID uuid.UUID, preset string, o MediaVariantOutcome) (bool, error) {
	var found bool
	err := r.withTenant(ctx, tenantID, func(q querier) error {
		var tag interface{ RowsAffected() int64 }
		var err error
		switch o.State {
		case domain.MediaVariantDone:
			tag, err = q.Exec(ctx, `
				UPDATE media_variants SET state = $4, storage_key = $5, content_type = $6,
					size_bytes = $7, width_px = $8, height_px = $9,
					next_attempt_at = NULL, error = NULL, skip_reason = NULL, updated_at = NOW()
				WHERE tenant_id = $1 AND asset_id = $2 AND preset = $3`,
				tenantID, assetID, preset, o.State, o.StorageKey, o.ContentType,
				o.SizeBytes, o.WidthPx, o.HeightPx)
		case domain.MediaVariantSkipped:
			tag, err = q.Exec(ctx, `
				UPDATE media_variants SET state = $4, skip_reason = $5,
					next_attempt_at = NULL, error = NULL, updated_at = NOW()
				WHERE tenant_id = $1 AND asset_id = $2 AND preset = $3`,
				tenantID, assetID, preset, o.State, o.SkipReason)
		case domain.MediaVariantPending, domain.MediaVariantFailed:
			tag, err = q.Exec(ctx, `
				UPDATE media_variants SET state = $4, attempts = attempts + 1,
					next_attempt_at = $5, error = $6, updated_at = NOW()
				WHERE tenant_id = $1 AND asset_id = $2 AND preset = $3`,
				tenantID, assetID, preset, o.State, o.NextAttemptAt, o.Error)
		default:
			return fmt.Errorf("unknown media variant state %q", o.State)
		}
		if err != nil {
			return err
		}
		found = tag.RowsAffected() > 0
		return nil
	})
	if err != nil {
		return false, fmt.Errorf("finish media variant: %w", err)
	}
	return found, nil
}
