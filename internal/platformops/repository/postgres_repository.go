package repository

import (
	"context"
	"fmt"
	"strings"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/williamlabdev/saas-forge/internal/platformops/domain"
)

type PostgresPlatformAppRepository struct {
	pool *pgxpool.Pool
}

func NewPostgresPlatformAppRepository(pool *pgxpool.Pool) *PostgresPlatformAppRepository {
	return &PostgresPlatformAppRepository{pool: pool}
}

func (r *PostgresPlatformAppRepository) List(ctx context.Context, f ListFilter) (ListResult, error) {
	limit := f.Limit
	if limit <= 0 || limit > 100 {
		limit = 20
	}
	offset := f.Offset
	if offset < 0 {
		offset = 0
	}

	var (
		conds  []string
		args   []any
		argPos = 1
	)
	conds = append(conds, "1=1")

	if q := strings.TrimSpace(f.Query); q != "" {
		conds = append(conds, fmt.Sprintf(
			"(LOWER(name) LIKE $%d OR LOWER(tenant_id) LIKE $%d)",
			argPos, argPos,
		))
		pattern := "%" + strings.ToLower(q) + "%"
		args = append(args, pattern)
		argPos++
	}
	if st := strings.TrimSpace(f.Status); st != "" && domain.ValidStatus(st) {
		conds = append(conds, fmt.Sprintf("status = $%d", argPos))
		args = append(args, st)
		argPos++
	}

	where := strings.Join(conds, " AND ")

	var total int
	countSQL := "SELECT COUNT(*) FROM platform_apps WHERE " + where
	if err := r.pool.QueryRow(ctx, countSQL, args...).Scan(&total); err != nil {
		return ListResult{}, fmt.Errorf("platform_apps count: %w", err)
	}

	listSQL := fmt.Sprintf(`
		SELECT id, name, tenant_id, owner, status, created_at, updated_at
		FROM platform_apps
		WHERE %s
		ORDER BY created_at DESC
		LIMIT $%d OFFSET $%d`, where, argPos, argPos+1)
	args = append(args, limit, offset)

	rows, err := r.pool.Query(ctx, listSQL, args...)
	if err != nil {
		return ListResult{}, fmt.Errorf("platform_apps list: %w", err)
	}
	defer rows.Close()

	var items []domain.PlatformApp
	for rows.Next() {
		var a domain.PlatformApp
		if err := rows.Scan(
			&a.ID, &a.Name, &a.TenantID, &a.Owner, &a.Status, &a.CreatedAt, &a.UpdatedAt,
		); err != nil {
			return ListResult{}, fmt.Errorf("platform_apps scan: %w", err)
		}
		items = append(items, a)
	}
	if err := rows.Err(); err != nil {
		return ListResult{}, err
	}
	return ListResult{Items: items, Total: total}, nil
}

func (r *PostgresPlatformAppRepository) Create(ctx context.Context, app *domain.PlatformApp) error {
	_, err := r.pool.Exec(ctx, `
		INSERT INTO platform_apps (id, name, tenant_id, owner, status, created_at, updated_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7)`,
		app.ID, app.Name, app.TenantID, app.Owner, app.Status, app.CreatedAt, app.UpdatedAt,
	)
	return err
}

func (r *PostgresPlatformAppRepository) UpdateStatus(ctx context.Context, id uuid.UUID, status string) (*domain.PlatformApp, error) {
	row := r.pool.QueryRow(ctx, `
		UPDATE platform_apps
		SET status = $2, updated_at = NOW()
		WHERE id = $1
		RETURNING id, name, tenant_id, owner, status, created_at, updated_at`,
		id, status,
	)
	var a domain.PlatformApp
	err := row.Scan(&a.ID, &a.Name, &a.TenantID, &a.Owner, &a.Status, &a.CreatedAt, &a.UpdatedAt)
	if err == pgx.ErrNoRows {
		return nil, fmt.Errorf("platform app not found")
	}
	if err != nil {
		return nil, err
	}
	return &a, nil
}
