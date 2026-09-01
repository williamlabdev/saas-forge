package repository

import (
	"context"

	"github.com/google/uuid"
	"github.com/williamlabdev/saas-forge/internal/ticket/domain"
)

type ListFilter struct {
	OwnerID uuid.UUID
	Limit   int
	Offset  int
}

type ListResult struct {
	Items []*domain.Ticket
	Total int
}

type TicketRepository interface {
	Create(ctx context.Context, m *domain.Ticket) error
	GetByID(ctx context.Context, id uuid.UUID) (*domain.Ticket, error)
	List(ctx context.Context, f ListFilter) (ListResult, error)
	Update(ctx context.Context, m *domain.Ticket) error
	Delete(ctx context.Context, id uuid.UUID) error
}
