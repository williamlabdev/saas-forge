package domain

import (
	"time"

	"github.com/google/uuid"
)

const (
	StatusActive   = "active"
	StatusArchived = "archived"
)

// Integration outbox event types for this aggregate.
const (
	EventTicketCreated = "ticket.created"
	EventTicketUpdated = "ticket.updated"
	EventTicketDeleted = "ticket.deleted"
)

type Ticket struct {
	ID        uuid.UUID
	OwnerID   uuid.UUID
	Name      string
	Status    string
	Version   int
	CreatedAt time.Time
	UpdatedAt time.Time
}

func ValidStatus(s string) bool {
	switch s {
	case StatusActive, StatusArchived:
		return true
	default:
		return false
	}
}
