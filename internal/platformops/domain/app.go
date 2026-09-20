package domain

import (
	"time"

	"github.com/google/uuid"
)

const (
	StatusActive   = "active"
	StatusPaused   = "paused"
	StatusArchived = "archived"
)

type PlatformApp struct {
	ID        uuid.UUID
	Name      string
	TenantID  string
	Owner     string
	Status    string
	CreatedAt time.Time
	UpdatedAt time.Time
}

func ValidStatus(s string) bool {
	switch s {
	case StatusActive, StatusPaused, StatusArchived:
		return true
	default:
		return false
	}
}
