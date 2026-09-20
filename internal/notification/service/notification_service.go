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
	// ListMine returns the caller's mailbox, scoped to their current tenant
	// (task 2). unread narrows to unread-only.
	ListMine(ctx context.Context, limit int, unread bool) ([]NotificationDTO, error)
	// UnreadCount is ListMine's unread total without paging through the rows —
	// what the console chrome polls for a badge.
	UnreadCount(ctx context.Context) (int, error)
	Create(ctx context.Context, in CreateInput) (NotificationDTO, error)
	// MarkRead marks one of the caller's OWN notifications read.
	// apperrors.ErrNotFound both when id does not exist and when it belongs to
	// someone else — the boundary this endpoint must not leak across.
	MarkRead(ctx context.Context, id uuid.UUID) (NotificationDTO, error)
	// MarkAllRead marks every unread notification in the caller's CURRENT
	// tenant's view read, and returns how many changed.
	MarkAllRead(ctx context.Context) (int, error)
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

// currentTenant parses sub.TenantID into a *uuid.UUID for the repository
// filter — nil when the subject has no active tenant (see
// repository.ListFilter's doc) or the claim is not a well-formed uuid (an
// agent or public-delivery subject, whose TenantID is not this shape); either
// way the caller falls back to seeing only tenant-less rows rather than
// erroring, which matches "notifications are a convenience, never a gate".
func currentTenant(sub authn.Subject) *uuid.UUID {
	if sub.TenantID == "" {
		return nil
	}
	id, err := uuid.Parse(sub.TenantID)
	if err != nil {
		return nil
	}
	return &id
}

func (s *notificationService) authorizeSelf(ctx context.Context, action string) (authn.Subject, error) {
	sub, ok := authn.SubjectFromContext(ctx)
	if !ok {
		return authn.Subject{}, apperrors.ErrUnauthorized
	}
	if err := s.authz.Allow(ctx, authz.Input{
		Action: action,
		Resource: authz.Resource{
			Type: "notification",
			ID:   sub.UserID.String(),
		},
	}); err != nil {
		return authn.Subject{}, err
	}
	return sub, nil
}

func (s *notificationService) ListMine(ctx context.Context, limit int, unread bool) ([]NotificationDTO, error) {
	sub, err := s.authorizeSelf(ctx, authz.ActionNotificationRead)
	if err != nil {
		return nil, err
	}
	rows, err := s.repo.ListForUser(ctx, sub.UserID, repository.ListFilter{
		TenantID:   currentTenant(sub),
		UnreadOnly: unread,
		Limit:      limit,
	})
	if err != nil {
		return nil, err
	}
	out := make([]NotificationDTO, len(rows))
	for i, n := range rows {
		out[i] = toDTO(n)
	}
	return out, nil
}

func (s *notificationService) UnreadCount(ctx context.Context) (int, error) {
	sub, err := s.authorizeSelf(ctx, authz.ActionNotificationRead)
	if err != nil {
		return 0, err
	}
	return s.repo.UnreadCount(ctx, sub.UserID, currentTenant(sub))
}

func (s *notificationService) Create(ctx context.Context, in CreateInput) (NotificationDTO, error) {
	if err := validate.Struct(in); err != nil {
		return NotificationDTO{}, apperrors.Wrap("VALIDATION_FAILED", err.Error(), 400, err)
	}
	sub, err := s.authorizeSelf(ctx, authz.ActionNotificationCreate)
	if err != nil {
		return NotificationDTO{}, err
	}
	now := time.Now().UTC()
	n := &domain.Notification{
		ID:        uuid.New(),
		UserID:    sub.UserID,
		Title:     in.Title,
		Body:      in.Body,
		Kind:      domain.KindGeneral,
		CreatedAt: now,
	}
	if err := s.repo.Create(ctx, n); err != nil {
		return NotificationDTO{}, err
	}
	return toDTO(n), nil
}

func (s *notificationService) MarkRead(ctx context.Context, id uuid.UUID) (NotificationDTO, error) {
	// Authorized against the CALLER's own id, same as every other verb here —
	// there is no "mark someone else's notification read" shape to authorize,
	// which is exactly why the repository, not this check, is what turns
	// another user's id into apperrors.ErrNotFound (see MarkRead's doc).
	sub, err := s.authorizeSelf(ctx, authz.ActionNotificationMarkRead)
	if err != nil {
		return NotificationDTO{}, err
	}
	n, err := s.repo.MarkRead(ctx, sub.UserID, id)
	if err != nil {
		return NotificationDTO{}, err
	}
	return toDTO(n), nil
}

func (s *notificationService) MarkAllRead(ctx context.Context) (int, error) {
	sub, err := s.authorizeSelf(ctx, authz.ActionNotificationMarkRead)
	if err != nil {
		return 0, err
	}
	return s.repo.MarkAllRead(ctx, sub.UserID, currentTenant(sub))
}
