package service

import (
	"time"

	"__MODULE__/internal/__domain__/domain"
	"github.com/google/uuid"
)

type __Domain__DTO struct {
	ID        uuid.UUID `json:"id"`
	Name      string    `json:"name"`
	Status    string    `json:"status"`
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}

func toDTO(m *domain.__Domain__) __Domain__DTO {
	return __Domain__DTO{
		ID:        m.ID,
		Name:      m.Name,
		Status:    m.Status,
		CreatedAt: m.CreatedAt,
		UpdatedAt: m.UpdatedAt,
	}
}
