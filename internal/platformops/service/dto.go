package service

import (
	"time"

	"github.com/google/uuid"
	"github.com/williamlabdev/saas-forge/internal/platformops/domain"
)

type PlatformAppDTO struct {
	ID        uuid.UUID `json:"id"`
	Name      string    `json:"name"`
	TenantID  string    `json:"tenant_id"`
	Owner     string    `json:"owner"`
	Status    string    `json:"status"`
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}

func toDTO(a *domain.PlatformApp) PlatformAppDTO {
	return PlatformAppDTO{
		ID:        a.ID,
		Name:      a.Name,
		TenantID:  a.TenantID,
		Owner:     a.Owner,
		Status:    a.Status,
		CreatedAt: a.CreatedAt,
		UpdatedAt: a.UpdatedAt,
	}
}
