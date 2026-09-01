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
	Event__Domain__Created = "__domain__.created"
	Event__Domain__Updated = "__domain__.updated"
	Event__Domain__Deleted = "__domain__.deleted"
)

type __Domain__ struct {
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
