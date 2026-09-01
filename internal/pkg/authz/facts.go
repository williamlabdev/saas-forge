package authz

import (
	"context"

	"github.com/google/uuid"
)

// RoleFactsLoader loads IAM role facts from persistence (optional OPA enrichment).
type RoleFactsLoader interface {
	RolesForUser(ctx context.Context, userID uuid.UUID) ([]string, error)
}
