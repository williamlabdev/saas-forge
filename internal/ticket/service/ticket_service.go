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
	"github.com/williamlabdev/saas-forge/internal/ticket/domain"
	"github.com/williamlabdev/saas-forge/internal/ticket/repository"
)

// Authz action contracts for this domain. These mirror the stable string
// convention used by internal/pkg/authz (e.g. "user:read"). Add matching
// cases to the RBAC/OPA policies, or run in AUTHZ_MODE=allow during dev.
const (
	ActionTicketList   = "ticket:list"
	ActionTicketRead   = "ticket:read"
	ActionTicketCreate = "ticket:create"
	ActionTicketUpdate = "ticket:update"
	ActionTicketDelete = "ticket:delete"
)

const resourceType = "ticket"

type TicketService interface {
	List(ctx context.Context, in ListInput) (*ListResult, error)
	GetByID(ctx context.Context, id uuid.UUID) (TicketDTO, error)
	Create(ctx context.Context, in CreateInput) (TicketDTO, error)
	Update(ctx context.Context, in UpdateInput) (TicketDTO, error)
	Delete(ctx context.Context, id uuid.UUID) error
}

type ListInput struct {
	Limit  int
	Offset int
}

type ListResult struct {
	Items  []TicketDTO `json:"items"`
	Total  int         `json:"total"`
	Limit  int         `json:"limit"`
	Offset int         `json:"offset"`
}

type CreateInput struct {
	Name string `json:"name" validate:"required,min=1,max=200"`
}

type UpdateInput struct {
	ID     uuid.UUID `json:"id" validate:"required"`
	Name   string    `json:"name" validate:"required,min=1,max=200"`
	Status string    `json:"status" validate:"required"`
}

type ticketService struct {
	repo  repository.TicketRepository
	authz authz.Authorizer
}

func NewTicketService(repo repository.TicketRepository, authz authz.Authorizer) TicketService {
	return &ticketService{repo: repo, authz: authz}
}

func (s *ticketService) authorize(ctx context.Context, action string, resourceID string) (authn.Subject, error) {
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

func (s *ticketService) List(ctx context.Context, in ListInput) (*ListResult, error) {
	sub, err := s.authorize(ctx, ActionTicketList, "collection")
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
	items := make([]TicketDTO, len(res.Items))
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

func (s *ticketService) GetByID(ctx context.Context, id uuid.UUID) (TicketDTO, error) {
	sub, err := s.authorize(ctx, ActionTicketRead, id.String())
	if err != nil {
		return TicketDTO{}, err
	}
	m, err := s.repo.GetByID(ctx, id)
	if err != nil {
		return TicketDTO{}, err
	}
	// Instance-level ownership backstop. The authorizer above decides the VERB
	// ("may this subject read tickets?") — it is never handed the record, so it
	// cannot decide THIS record, and in AUTHZ_MODE=allow it decides nothing at
	// all. owner_id is written on create and filtered on list; without the same
	// comparison here, these three by-id operations are the hole in what is
	// otherwise a per-owner resource. Not-found rather than forbidden: a 403 on
	// someone else's id confirms that the id exists.
	if m.OwnerID != sub.UserID {
		return TicketDTO{}, apperrors.ErrNotFound
	}
	return toDTO(m), nil
}

func (s *ticketService) Create(ctx context.Context, in CreateInput) (TicketDTO, error) {
	if err := validate.Struct(in); err != nil {
		return TicketDTO{}, apperrors.Wrap("VALIDATION_FAILED", err.Error(), 400, err)
	}
	sub, err := s.authorize(ctx, ActionTicketCreate, "collection")
	if err != nil {
		return TicketDTO{}, err
	}
	now := time.Now().UTC()
	m := &domain.Ticket{
		ID:        uuid.New(),
		OwnerID:   sub.UserID,
		Name:      strings.TrimSpace(in.Name),
		Status:    domain.StatusActive,
		CreatedAt: now,
		UpdatedAt: now,
	}
	if err := s.repo.Create(ctx, m); err != nil {
		return TicketDTO{}, err
	}
	return toDTO(m), nil
}

func (s *ticketService) Update(ctx context.Context, in UpdateInput) (TicketDTO, error) {
	if err := validate.Struct(in); err != nil {
		return TicketDTO{}, apperrors.Wrap("VALIDATION_FAILED", err.Error(), 400, err)
	}
	sub, err := s.authorize(ctx, ActionTicketUpdate, in.ID.String())
	if err != nil {
		return TicketDTO{}, err
	}
	status := strings.TrimSpace(in.Status)
	if !domain.ValidStatus(status) {
		return TicketDTO{}, apperrors.Wrap("VALIDATION_FAILED", "invalid status", 400, nil)
	}
	m, err := s.repo.GetByID(ctx, in.ID)
	if err != nil {
		return TicketDTO{}, err
	}
	// Same backstop as GetByID.
	if m.OwnerID != sub.UserID {
		return TicketDTO{}, apperrors.ErrNotFound
	}
	m.Name = strings.TrimSpace(in.Name)
	m.Status = status
	m.UpdatedAt = time.Now().UTC()
	if err := s.repo.Update(ctx, m); err != nil {
		return TicketDTO{}, err
	}
	return toDTO(m), nil
}

func (s *ticketService) Delete(ctx context.Context, id uuid.UUID) error {
	sub, err := s.authorize(ctx, ActionTicketDelete, id.String())
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
