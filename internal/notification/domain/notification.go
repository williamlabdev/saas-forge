package domain

import (
	"time"

	"github.com/google/uuid"
)

type Notification struct {
	ID        uuid.UUID
	UserID    uuid.UUID
	Title     string
	Body      string
	ReadAt    *time.Time
	CreatedAt time.Time
}
