package repository

import (
	"context"

	"__MODULE__/internal/__domain__/domain"
	"github.com/google/uuid"
)

type ListFilter struct {
	OwnerID uuid.UUID
	Limit   int
	Offset  int
}

type ListResult struct {
	Items []*domain.__Domain__
	Total int
}

type __Domain__Repository interface {
	Create(ctx context.Context, m *domain.__Domain__) error
	GetByID(ctx context.Context, id uuid.UUID) (*domain.__Domain__, error)
	List(ctx context.Context, f ListFilter) (ListResult, error)
	Update(ctx context.Context, m *domain.__Domain__) error
	Delete(ctx context.Context, id uuid.UUID) error
}
