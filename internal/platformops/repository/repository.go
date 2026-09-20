package repository

import (
	"context"

	"github.com/google/uuid"
	"github.com/williamlabdev/saas-forge/internal/platformops/domain"
)

type ListFilter struct {
	Query  string
	Status string
	Limit  int
	Offset int
}

type ListResult struct {
	Items []domain.PlatformApp
	Total int
}

type PlatformAppRepository interface {
	List(ctx context.Context, f ListFilter) (ListResult, error)
	Create(ctx context.Context, app *domain.PlatformApp) error
	UpdateStatus(ctx context.Context, id uuid.UUID, status string) (*domain.PlatformApp, error)
}
