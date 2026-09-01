package repository

import (
	"context"

	"github.com/google/uuid"
	"github.com/williamlabdev/saas-forge/internal/pkg/pagination"
	"github.com/williamlabdev/saas-forge/internal/user/domain"
)

// UserRepository abstracts user persistence (arch-user.md §9.1).
type UserRepository interface {
	Create(ctx context.Context, u *domain.User, passwordHash, idempotencyKey string) error
	ByID(ctx context.Context, id uuid.UUID) (*domain.User, error)
	ByEmailHash(ctx context.Context, emailHash []byte) (*domain.User, error)
	ByUsernameHash(ctx context.Context, usernameHash []byte) (*domain.User, error)
	Update(ctx context.Context, u *domain.User) error
	UpdatePreferences(ctx context.Context, id uuid.UUID, prefs domain.Preferences, merge bool) error
	SoftDelete(ctx context.Context, id uuid.UUID) error
	PublishUserUpdated(ctx context.Context, u *domain.User) error
	PublishUserDeleted(ctx context.Context, u *domain.User) error
	ListPage(ctx context.Context, statusFilter string, cursor *pagination.UserCursor, limit int) ([]*domain.User, error)
}
