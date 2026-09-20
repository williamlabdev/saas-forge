package domain

import (
	"time"

	"github.com/google/uuid"
)

type Status string

const (
	StatusActive    Status = "active"
	StatusSuspended Status = "suspended"
	StatusDeleted   Status = "deleted"
)

// Preferences is a flexible JSONB document (validated in service layer).
type Preferences map[string]any

// User holds plaintext PII inside the service/repository boundary only.
type User struct {
	ID                 uuid.UUID
	Username           string
	Email              string
	DisplayName        string
	Phone              string
	Preferences        Preferences
	Status             Status
	StatusVersion      int
	UsernameLookupHash []byte
	EmailLookupHash    []byte
	CreatedAt          time.Time
	UpdatedAt          time.Time
	DeletedAt          *time.Time
}

func (u *User) IsActive() bool {
	return u.Status == StatusActive && u.DeletedAt == nil
}
