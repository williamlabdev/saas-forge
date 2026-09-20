package service

import (
	"time"

	"github.com/google/uuid"
	"github.com/williamlabdev/saas-forge/internal/notification/domain"
)

type NotificationDTO struct {
	ID    uuid.UUID `json:"id"`
	Title string    `json:"title"`
	Body  string     `json:"body"`
	// Kind classifies the notice — see domain.Kind*. Always present; rows
	// created before 2.5a read back as "general", the column's DEFAULT.
	Kind string `json:"kind"`
	// Link is an app-relative path the console can navigate to, or omitted
	// when the notice has none.
	Link *string `json:"link,omitempty"`
	// TenantID is omitted for a notice that names no tenant (see
	// domain.Notification.TenantID).
	TenantID  *uuid.UUID `json:"tenant_id,omitempty"`
	Read      bool       `json:"read"`
	CreatedAt time.Time  `json:"created_at"`
}

func toDTO(n *domain.Notification) NotificationDTO {
	return NotificationDTO{
		ID:        n.ID,
		Title:     n.Title,
		Body:      n.Body,
		Kind:      n.Kind,
		Link:      n.Link,
		TenantID:  n.TenantID,
		Read:      n.ReadAt != nil,
		CreatedAt: n.CreatedAt,
	}
}
