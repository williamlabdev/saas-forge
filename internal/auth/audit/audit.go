package audit

import (
	"context"

	"github.com/google/uuid"
)

// EventType identifies an auth audit event.
type EventType string

const (
	EventLogin        EventType = "login"
	EventRefresh      EventType = "refresh"
	EventLogout       EventType = "logout"
	EventTenantSwitch EventType = "tenant_switch"
)

// Outcome is success or failure.
type Outcome string

const (
	OutcomeSuccess Outcome = "success"
	OutcomeFailure Outcome = "failure"
)

// Entry is one append-only auth audit row.
type Entry struct {
	EventType EventType
	Outcome   Outcome
	UserID    *uuid.UUID
	ClientIP  string
	UserAgent string
	ErrorCode string
}

// Recorder persists auth audit events (best-effort; must not fail auth flows).
type Recorder interface {
	Record(ctx context.Context, e Entry) error
}

// NoopRecorder discards audit events (tests).
type NoopRecorder struct{}

func (NoopRecorder) Record(context.Context, Entry) error { return nil }
