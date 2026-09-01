package service

import (
	"context"
	"time"

	"github.com/google/uuid"
	"github.com/williamlabdev/saas-forge/internal/notification/domain"
	"github.com/williamlabdev/saas-forge/internal/notification/repository"
	"github.com/williamlabdev/saas-forge/internal/pkg/authn"
	"github.com/williamlabdev/saas-forge/internal/pkg/authz"
	apperrors "github.com/williamlabdev/saas-forge/internal/pkg/errors"
	"github.com/williamlabdev/saas-forge/internal/pkg/validate"
)

type NotificationService interface {
	ListMine(ctx context.Context, limit int) ([]NotificationDTO, error)
	Create(ctx context.Context, in CreateInput) (NotificationDTO, error)
}

type CreateInput struct {
	Title string `validate:"required,min=1,max=200"`
	Body  string `validate:"required,min=1,max=2000"`
}

type notificationService struct {
	repo  repository.NotificationRepository
	authz authz.Authorizer
}

func NewNotificationService(repo repository.NotificationRepository, authz authz.Authorizer) NotificationService {
	return &notificationService{repo: repo, authz: authz}
}

func (s *notificationService) ListMine(ctx context.Context, limit int) ([]NotificationDTO, error) {
	sub, ok := authn.SubjectFromContext(ctx)
	if !ok {
		return nil, apperrors.ErrUnauthorized
	}
	if err := s.authz.Allow(ctx, authz.Input{
		Action: authz.ActionNotificationRead,
		Resource: authz.Resource{
			Type: "notification",
			ID:   sub.UserID.String(),
		},
	}); err != nil {
		return nil, err
	}
	rows, err := s.repo.ListForUser(ctx, sub.UserID, limit)
	if err != nil {
		return nil, err
	}
	out := make([]NotificationDTO, len(rows))
	for i, n := range rows {
		out[i] = toDTO(n)
	}
	return out, nil
}

func (s *notificationService) Create(ctx context.Context, in CreateInput) (NotificationDTO, error) {
	if err := validate.Struct(in); err != nil {
		return NotificationDTO{}, apperrors.Wrap("VALIDATION_FAILED", err.Error(), 400, err)
	}
	sub, ok := authn.SubjectFromContext(ctx)
	if !ok {
		return NotificationDTO{}, apperrors.ErrUnauthorized
	}
	if err := s.authz.Allow(ctx, authz.Input{
		Action: authz.ActionNotificationCreate,
		Resource: authz.Resource{
			Type: "notification",
			ID:   sub.UserID.String(),
		},
	}); err != nil {
		return NotificationDTO{}, err
	}
	now := time.Now().UTC()
	n := &domain.Notification{
		ID:        uuid.New(),
		UserID:    sub.UserID,
		Title:     in.Title,
		Body:      in.Body,
		CreatedAt: now,
	}
	if err := s.repo.Create(ctx, n); err != nil {
		return NotificationDTO{}, err
	}
	return toDTO(n), nil
}
