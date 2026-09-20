package pagination

import (
	"encoding/base64"
	"encoding/json"
	"time"

	"github.com/google/uuid"
	apperrors "github.com/williamlabdev/saas-forge/internal/pkg/errors"
)

// PageMeta is returned in response meta for cursor-based lists.
type PageMeta struct {
	Limit      int    `json:"limit"`
	NextCursor string `json:"next_cursor,omitempty"`
	HasMore    bool   `json:"has_more"`
}

// UserCursor is the keyset position for users (created_at DESC, id DESC).
type UserCursor struct {
	CreatedAt time.Time `json:"created_at"`
	ID        uuid.UUID `json:"id"`
}

// EncodeUserCursor returns an opaque URL-safe cursor token.
func EncodeUserCursor(c UserCursor) (string, error) {
	b, err := json.Marshal(c)
	if err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}

// DecodeUserCursor parses a cursor token from the client.
func DecodeUserCursor(raw string) (UserCursor, error) {
	if raw == "" {
		return UserCursor{}, apperrors.New("INVALID_CURSOR", "invalid pagination cursor", 400)
	}
	b, err := base64.RawURLEncoding.DecodeString(raw)
	if err != nil {
		return UserCursor{}, apperrors.New("INVALID_CURSOR", "invalid pagination cursor", 400)
	}
	var c UserCursor
	if err := json.Unmarshal(b, &c); err != nil || c.ID == uuid.Nil || c.CreatedAt.IsZero() {
		return UserCursor{}, apperrors.New("INVALID_CURSOR", "invalid pagination cursor", 400)
	}
	return c, nil
}

// ClampLimit bounds page size (default 20, max 100).
func ClampLimit(n int) int {
	switch {
	case n <= 0:
		return 20
	case n > 100:
		return 100
	default:
		return n
	}
}
