package outbox

import (
	"encoding/json"
	"fmt"

	"github.com/google/uuid"
)

const (
	EventUserCreated = "user.created"
	EventUserUpdated = "user.updated"
	EventUserDeleted = "user.deleted"
)

// UserPayload is the MCP upsert contract (minimal fields).
type UserPayload struct {
	UserID        string `json:"user_id"`
	Status        string `json:"status"`
	StatusVersion int    `json:"status_version"`
	Username      string `json:"username,omitempty"`
}

// IdempotencyKey returns user_id + status_version per HLD.
func IdempotencyKey(userID uuid.UUID, version int) string {
	return fmt.Sprintf("%s:%d", userID.String(), version)
}

func MarshalPayload(p UserPayload) (json.RawMessage, error) {
	b, err := json.Marshal(p)
	if err != nil {
		return nil, err
	}
	return b, nil
}
