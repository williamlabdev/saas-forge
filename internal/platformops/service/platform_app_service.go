package service

import (
	"context"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/williamlabdev/saas-forge/internal/pkg/authn"
	"github.com/williamlabdev/saas-forge/internal/pkg/authz"
	apperrors "github.com/williamlabdev/saas-forge/internal/pkg/errors"
	"github.com/williamlabdev/saas-forge/internal/pkg/validate"
	"github.com/williamlabdev/saas-forge/internal/platformops/domain"
	"github.com/williamlabdev/saas-forge/internal/platformops/repository"
)

type PlatformAppService interface {
	List(ctx context.Context, in ListInput) (*ListResult, error)
	Create(ctx context.Context, in CreateInput) (PlatformAppDTO, error)
	UpdateStatus(ctx context.Context, in UpdateStatusInput) (PlatformAppDTO, error)
}

type ListInput struct {
	Query  string
	Status string
	Limit  int
	Offset int
}

type ListResult struct {
	Items  []PlatformAppDTO `json:"items"`
	Total  int              `json:"total"`
	Limit  int              `json:"limit"`
	Offset int              `json:"offset"`
}

type CreateInput struct {
	Name     string `json:"name" validate:"required,min=1,max=200"`
	TenantID string `json:"tenant_id" validate:"required,min=1,max=100"`
	Owner    string `json:"owner" validate:"omitempty,max=200"`
}

type UpdateStatusInput struct {
	ID     uuid.UUID `json:"id" validate:"required"`
	Status string    `json:"status" validate:"required"`
}

type platformAppService struct {
	repo  repository.PlatformAppRepository
	authz authz.Authorizer
}

func NewPlatformAppService(repo repository.PlatformAppRepository, authz authz.Authorizer) PlatformAppService {
	return &platformAppService{repo: repo, authz: authz}
}

func (s *platformAppService) allowPlatform(ctx context.Context, action string) error {
	if _, ok := authn.SubjectFromContext(ctx); !ok {
		return apperrors.ErrUnauthorized
	}
	return s.authz.Allow(ctx, authz.Input{
		Action: action,
		Resource: authz.Resource{
			Type: "platform_apps",
			ID:   "collection",
		},
	})
}

func (s *platformAppService) List(ctx context.Context, in ListInput) (*ListResult, error) {
	if err := s.allowPlatform(ctx, authz.ActionPlatformAppList); err != nil {
		return nil, err
	}
	res, err := s.repo.List(ctx, repository.ListFilter{
		Query:  in.Query,
		Status: in.Status,
		Limit:  in.Limit,
		Offset: in.Offset,
	})
	if err != nil {
		return nil, err
	}
	items := make([]PlatformAppDTO, len(res.Items))
	for i := range res.Items {
		items[i] = toDTO(&res.Items[i])
	}
	limit := in.Limit
	if limit <= 0 || limit > 100 {
		limit = 20
	}
	return &ListResult{
		Items:  items,
		Total:  res.Total,
		Limit:  limit,
		Offset: in.Offset,
	}, nil
}

func (s *platformAppService) Create(ctx context.Context, in CreateInput) (PlatformAppDTO, error) {
	if err := validate.Struct(in); err != nil {
		return PlatformAppDTO{}, apperrors.Wrap("VALIDATION_FAILED", err.Error(), 400, err)
	}
	if err := s.allowPlatform(ctx, authz.ActionPlatformAppCreate); err != nil {
		return PlatformAppDTO{}, err
	}
	sub, ok := authn.SubjectFromContext(ctx)
	if !ok {
		return PlatformAppDTO{}, apperrors.ErrUnauthorized
	}
	owner := strings.TrimSpace(in.Owner)
	if owner == "" {
		owner = sub.UserID.String()
	}
	now := time.Now().UTC()
	app := &domain.PlatformApp{
		ID:        uuid.New(),
		Name:      strings.TrimSpace(in.Name),
		TenantID:  strings.TrimSpace(in.TenantID),
		Owner:     owner,
		Status:    domain.StatusActive,
		CreatedAt: now,
		UpdatedAt: now,
	}
	if err := s.repo.Create(ctx, app); err != nil {
		return PlatformAppDTO{}, err
	}
	return toDTO(app), nil
}

func (s *platformAppService) UpdateStatus(ctx context.Context, in UpdateStatusInput) (PlatformAppDTO, error) {
	if err := validate.Struct(in); err != nil {
		return PlatformAppDTO{}, apperrors.Wrap("VALIDATION_FAILED", err.Error(), 400, err)
	}
	if err := s.allowPlatform(ctx, authz.ActionPlatformAppUpdate); err != nil {
		return PlatformAppDTO{}, err
	}
	st := strings.TrimSpace(in.Status)
	if !domain.ValidStatus(st) {
		return PlatformAppDTO{}, apperrors.Wrap("VALIDATION_FAILED", "invalid status", 400, nil)
	}
	updated, err := s.repo.UpdateStatus(ctx, in.ID, st)
	if err != nil {
		if strings.Contains(err.Error(), "not found") {
			return PlatformAppDTO{}, apperrors.Wrap("NOT_FOUND", "platform app not found", 404, err)
		}
		return PlatformAppDTO{}, err
	}
	return toDTO(updated), nil
}
