package mcp

import (
	"context"

	"github.com/google/uuid"
)

// UpsertRequest is sent to the MCP user-state API (idempotent).
type UpsertRequest struct {
	UserID         uuid.UUID
	Status         string
	StatusVersion  int
	IdempotencyKey string
	EventType      string
}

// Client pushes user state to the MCP service.
type Client interface {
	UpsertUserState(ctx context.Context, req UpsertRequest) error
}
