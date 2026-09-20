package repository

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/williamlabdev/saas-forge/internal/iam/domain"
	apperrors "github.com/williamlabdev/saas-forge/internal/pkg/errors"
)

// PostgresIAMRepository implements IAMRepository.
type PostgresIAMRepository struct {
	pool *pgxpool.Pool
}

func NewPostgresIAMRepository(pool *pgxpool.Pool) *PostgresIAMRepository {
	return &PostgresIAMRepository{pool: pool}
}

func (r *PostgresIAMRepository) RoleByName(ctx context.Context, name string) (*domain.Role, error) {
	var role domain.Role
	err := r.pool.QueryRow(ctx, `
		SELECT id, name, description, created_at FROM roles WHERE name = $1
	`, name).Scan(&role.ID, &role.Name, &role.Description, &role.CreatedAt)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, apperrors.ErrNotFound
		}
		return nil, fmt.Errorf("iam: role by name: %w", err)
	}
	return &role, nil
}

func (r *PostgresIAMRepository) RolesForUser(ctx context.Context, userID uuid.UUID) ([]string, error) {
	rows, err := r.pool.Query(ctx, `
		SELECT r.name
		FROM user_roles ur
		JOIN roles r ON r.id = ur.role_id
		WHERE ur.user_id = $1
		  AND (ur.expires_at IS NULL OR ur.expires_at > now())
	`, userID)
	if err != nil {
		return nil, fmt.Errorf("iam: roles for user: %w", err)
	}
	defer rows.Close()

	var names []string
	for rows.Next() {
		var n string
		if err := rows.Scan(&n); err != nil {
			return nil, err
		}
		names = append(names, n)
	}
	return names, rows.Err()
}

func (r *PostgresIAMRepository) RevokeRole(ctx context.Context, userID, roleID uuid.UUID) error {
	_, err := r.pool.Exec(ctx, `
		DELETE FROM user_roles WHERE user_id = $1 AND role_id = $2
	`, userID, roleID)
	if err != nil {
		return fmt.Errorf("iam: revoke role: %w", err)
	}
	return nil
}

func (r *PostgresIAMRepository) AssignRole(ctx context.Context, userID, roleID uuid.UUID) error {
	_, err := r.pool.Exec(ctx, `
		INSERT INTO user_roles (user_id, role_id, created_at)
		VALUES ($1, $2, $3)
		ON CONFLICT (user_id, role_id) DO NOTHING
	`, userID, roleID, time.Now().UTC())
	if err != nil {
		return fmt.Errorf("iam: assign role: %w", err)
	}
	return nil
}
