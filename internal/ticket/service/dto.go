package service

import (
	"time"

	"github.com/google/uuid"
	"github.com/williamlabdev/saas-forge/internal/ticket/domain"
)

type TicketDTO struct {
	ID        uuid.UUID `json:"id"`
	Name      string    `json:"name"`
	Status    string    `json:"status"`
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}

func toDTO(m *domain.Ticket) TicketDTO {
	return TicketDTO{
		ID:        m.ID,
		Name:      m.Name,
		Status:    m.Status,
		CreatedAt: m.CreatedAt,
		UpdatedAt: m.UpdatedAt,
	}
}
