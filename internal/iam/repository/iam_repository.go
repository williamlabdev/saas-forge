package repository

import (
	"context"

	"github.com/google/uuid"
	"github.com/williamlabdev/saas-forge/internal/iam/domain"
)

// IAMRepository loads and mutates IAM facts.
type IAMRepository interface {
	RoleByName(ctx context.Context, name string) (*domain.Role, error)
	RolesForUser(ctx context.Context, userID uuid.UUID) ([]string, error)
	AssignRole(ctx context.Context, userID, roleID uuid.UUID) error
	RevokeRole(ctx context.Context, userID, roleID uuid.UUID) error
}
