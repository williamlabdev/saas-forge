package service

import (
	"context"
	"strings"
	"time"

	"__MODULE__/internal/__domain__/domain"
	"__MODULE__/internal/__domain__/repository"
	"__MODULE__/internal/pkg/authn"
	"__MODULE__/internal/pkg/authz"
	apperrors "__MODULE__/internal/pkg/errors"
	"__MODULE__/internal/pkg/validate"
	"github.com/google/uuid"
)

// Authz action contracts for this domain. These mirror the stable string
// convention used by internal/pkg/authz (e.g. "user:read"). Add matching
// cases to the RBAC/OPA policies, or run in AUTHZ_MODE=allow during dev.
const (
	Action__Domain__List   = "__domain__:list"
	Action__Domain__Read   = "__domain__:read"
	Action__Domain__Create = "__domain__:create"
	Action__Domain__Update = "__domain__:update"
	Action__Domain__Delete = "__domain__:delete"
)

const resourceType = "__domain__"

type __Domain__Service interface {
	List(ctx context.Context, in ListInput) (*ListResult, error)
	GetByID(ctx context.Context, id uuid.UUID) (__Domain__DTO, error)
	Create(ctx context.Context, in CreateInput) (__Domain__DTO, error)
	Update(ctx context.Context, in UpdateInput) (__Domain__DTO, error)
	Delete(ctx context.Context, id uuid.UUID) error
}

type ListInput struct {
	Limit  int
	Offset int
}

type ListResult struct {
	Items  []__Domain__DTO `json:"items"`
	Total  int             `json:"total"`
	Limit  int             `json:"limit"`
	Offset int             `json:"offset"`
}

type CreateInput struct {
	Name string `json:"name" validate:"required,min=1,max=200"`
}

type UpdateInput struct {
	ID     uuid.UUID `json:"id" validate:"required"`
	Name   string    `json:"name" validate:"required,min=1,max=200"`
	Status string    `json:"status" validate:"required"`
}

type __domain__Service struct {
	repo  repository.__Domain__Repository
	authz authz.Authorizer
}

func New__Domain__Service(repo repository.__Domain__Repository, authz authz.Authorizer) __Domain__Service {
	return &__domain__Service{repo: repo, authz: authz}
}

func (s *__domain__Service) authorize(ctx context.Context, action string, resourceID string) (authn.Subject, error) {
	sub, ok := authn.SubjectFromContext(ctx)
	if !ok {
		return authn.Subject{}, apperrors.ErrUnauthorized
	}
	if err := s.authz.Allow(ctx, authz.Input{
		Action: action,
		Resource: authz.Resource{
			Type: resourceType,
			ID:   resourceID,
		},
	}); err != nil {
		return authn.Subject{}, err
	}
	return sub, nil
}

func (s *__domain__Service) List(ctx context.Context, in ListInput) (*ListResult, error) {
	sub, err := s.authorize(ctx, Action__Domain__List, "collection")
	if err != nil {
		return nil, err
	}
	res, err := s.repo.List(ctx, repository.ListFilter{
		OwnerID: sub.UserID,
		Limit:   in.Limit,
		Offset:  in.Offset,
	})
	if err != nil {
		return nil, err
	}
	items := make([]__Domain__DTO, len(res.Items))
	for i := range res.Items {
		items[i] = toDTO(res.Items[i])
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

func (s *__domain__Service) GetByID(ctx context.Context, id uuid.UUID) (__Domain__DTO, error) {
	sub, err := s.authorize(ctx, Action__Domain__Read, id.String())
	if err != nil {
		return __Domain__DTO{}, err
	}
	m, err := s.repo.GetByID(ctx, id)
	if err != nil {
		return __Domain__DTO{}, err
	}
	// Instance-level ownership backstop. The authorizer above decides the VERB
	// ("may this subject read __domain__s?") — it is never handed the record, so it
	// cannot decide THIS record, and in AUTHZ_MODE=allow it decides nothing at
	// all. owner_id is written on create and filtered on list; without the same
	// comparison here, these three by-id operations are the hole in what is
	// otherwise a per-owner resource. Not-found rather than forbidden: a 403 on
	// someone else's id confirms that the id exists.
	if m.OwnerID != sub.UserID {
		return __Domain__DTO{}, apperrors.ErrNotFound
	}
	return toDTO(m), nil
}

func (s *__domain__Service) Create(ctx context.Context, in CreateInput) (__Domain__DTO, error) {
	if err := validate.Struct(in); err != nil {
		return __Domain__DTO{}, apperrors.Wrap("VALIDATION_FAILED", err.Error(), 400, err)
	}
	sub, err := s.authorize(ctx, Action__Domain__Create, "collection")
	if err != nil {
		return __Domain__DTO{}, err
	}
	now := time.Now().UTC()
	m := &domain.__Domain__{
		ID:        uuid.New(),
		OwnerID:   sub.UserID,
		Name:      strings.TrimSpace(in.Name),
		Status:    domain.StatusActive,
		CreatedAt: now,
		UpdatedAt: now,
	}
	if err := s.repo.Create(ctx, m); err != nil {
		return __Domain__DTO{}, err
	}
	return toDTO(m), nil
}

func (s *__domain__Service) Update(ctx context.Context, in UpdateInput) (__Domain__DTO, error) {
	if err := validate.Struct(in); err != nil {
		return __Domain__DTO{}, apperrors.Wrap("VALIDATION_FAILED", err.Error(), 400, err)
	}
	sub, err := s.authorize(ctx, Action__Domain__Update, in.ID.String())
	if err != nil {
		return __Domain__DTO{}, err
	}
	status := strings.TrimSpace(in.Status)
	if !domain.ValidStatus(status) {
		return __Domain__DTO{}, apperrors.Wrap("VALIDATION_FAILED", "invalid status", 400, nil)
	}
	m, err := s.repo.GetByID(ctx, in.ID)
	if err != nil {
		return __Domain__DTO{}, err
	}
	// Same backstop as GetByID.
	if m.OwnerID != sub.UserID {
		return __Domain__DTO{}, apperrors.ErrNotFound
	}
	m.Name = strings.TrimSpace(in.Name)
	m.Status = status
	m.UpdatedAt = time.Now().UTC()
	if err := s.repo.Update(ctx, m); err != nil {
		return __Domain__DTO{}, err
	}
	return toDTO(m), nil
}

func (s *__domain__Service) Delete(ctx context.Context, id uuid.UUID) error {
	sub, err := s.authorize(ctx, Action__Domain__Delete, id.String())
	if err != nil {
		return err
	}
	// Delete now reads before it writes, purely to answer "is this yours?".
	m, err := s.repo.GetByID(ctx, id)
	if err != nil {
		return err
	}
	if m.OwnerID != sub.UserID {
		return apperrors.ErrNotFound
	}
	return s.repo.Delete(ctx, id)
}
