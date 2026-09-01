package service

import (
	"context"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/williamlabdev/saas-forge/internal/notification/domain"
	"github.com/williamlabdev/saas-forge/internal/notification/repository"
	"github.com/williamlabdev/saas-forge/internal/pkg/authn"
	"github.com/williamlabdev/saas-forge/internal/pkg/authz"
	apperrors "github.com/williamlabdev/saas-forge/internal/pkg/errors"
)

type memRepo struct {
	items []*domain.Notification
}

func (m *memRepo) Create(_ context.Context, n *domain.Notification) error {
	m.items = append(m.items, n)
	return nil
}

func (m *memRepo) ListForUser(_ context.Context, userID uuid.UUID, limit int) ([]*domain.Notification, error) {
	var out []*domain.Notification
	for _, it := range m.items {
		if it.UserID == userID {
			out = append(out, it)
		}
	}
	if len(out) > limit {
		out = out[:limit]
	}
	return out, nil
}

func TestNotificationService_CreateAndList(t *testing.T) {
	uid := uuid.New()
	repo := &memRepo{}
	svc := NewNotificationService(repo, authz.NewRBACAuthorizer())
	ctx := authn.WithSubject(context.Background(), authn.Subject{
		UserID: uid,
		Roles:  []string{"member"},
	})

	dto, err := svc.Create(ctx, CreateInput{Title: "Hi", Body: "Body"})
	require.NoError(t, err)
	assert.Equal(t, "Hi", dto.Title)

	list, err := svc.ListMine(ctx, 10)
	require.NoError(t, err)
	require.Len(t, list, 1)
}

func TestNotificationService_Unauthorized(t *testing.T) {
	svc := NewNotificationService(&memRepo{}, authz.NewRBACAuthorizer())
	_, err := svc.ListMine(context.Background(), 10)
	require.Error(t, err)
	ae, ok := apperrors.As(err)
	require.True(t, ok)
	assert.Equal(t, apperrors.ErrUnauthorized.Code, ae.Code)
}

var _ repository.NotificationRepository = (*memRepo)(nil)
