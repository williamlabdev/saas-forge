package domain

import (
	"time"

	"github.com/google/uuid"
)

// Role is a named IAM role (facts for OPA; not executable policy).
type Role struct {
	ID          uuid.UUID
	Name        string
	Description string
	CreatedAt   time.Time
}

// UserRoleAssignment links a user to a role.
type UserRoleAssignment struct {
	UserID    uuid.UUID
	RoleID    uuid.UUID
	RoleName  string
	ExpiresAt *time.Time
	CreatedAt time.Time
}
