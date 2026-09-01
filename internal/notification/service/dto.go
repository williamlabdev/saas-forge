package service

import (
	"time"

	"github.com/google/uuid"
	"github.com/williamlabdev/saas-forge/internal/notification/domain"
)

type NotificationDTO struct {
	ID        uuid.UUID `json:"id"`
	Title     string    `json:"title"`
	Body      string    `json:"body"`
	Read      bool      `json:"read"`
	CreatedAt time.Time `json:"created_at"`
}

func toDTO(n *domain.Notification) NotificationDTO {
	return NotificationDTO{
		ID:        n.ID,
		Title:     n.Title,
		Body:      n.Body,
		Read:      n.ReadAt != nil,
		CreatedAt: n.CreatedAt,
	}
}
