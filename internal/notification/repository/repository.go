package repository

import (
	"context"

	"github.com/google/uuid"
	"github.com/williamlabdev/saas-forge/internal/notification/domain"
)

type NotificationRepository interface {
	Create(ctx context.Context, n *domain.Notification) error
	ListForUser(ctx context.Context, userID uuid.UUID, limit int) ([]*domain.Notification, error)
}
