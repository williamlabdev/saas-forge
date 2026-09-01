package service

import (
	"encoding/base64"
	"encoding/json"
	"time"

	"github.com/google/uuid"
	"github.com/williamlabdev/saas-forge/internal/cms/content/repository"
	apperrors "github.com/williamlabdev/saas-forge/internal/pkg/errors"
)

// The cursor is deliberately OPAQUE to callers: base64url over a JSON pair.
// Opaque is not obfuscation — it is the only thing that lets the sort key change
// later (add a tiebreaker, switch the ordering column) without every consumer
// that hand-built a token breaking. The API contract is "hand back exactly what
// you were given"; nothing else about the token is promised.
//
// It is NOT signed, and does not need to be. It encodes only created_at and an
// entry id — both of which the same caller can already read off the page it came
// from — and it can only ever move a scan window WITHIN the result set the
// caller's own credential already scopes (tenant + type + status=published are
// applied independently of the cursor). Forging one buys nothing.
type cursorPayload struct {
	// Field names are short because this ends up in URLs; T is RFC3339 with
	// nanoseconds so the round-trip is lossless against a Postgres timestamptz.
	T string `json:"t"`
	I string `json:"i"`
}

// ErrCursorInvalid is a malformed or tampered cursor. It is a 400 rather than a
// silent "start from the beginning": quietly restarting a page-through loop is
// how a consumer ends up in an infinite loop re-reading page 1 forever.
var ErrCursorInvalid = apperrors.New("CONTENT_CURSOR_INVALID", "cursor is not a cursor this API issued", 400)

func encodeCursor(createdAt time.Time, id uuid.UUID) string {
	raw, err := json.Marshal(cursorPayload{
		T: createdAt.UTC().Format(time.RFC3339Nano),
		I: id.String(),
	})
	if err != nil {
		// Unreachable: both fields are strings. Returning "" degrades to "no
		// next page", which is the fail-closed direction — a caller stops early
		// rather than looping on a cursor that does not advance.
		return ""
	}
	return base64.RawURLEncoding.EncodeToString(raw)
}

func decodeCursor(raw string) (*repository.EntryCursor, error) {
	if raw == "" {
		return nil, nil
	}
	b, err := base64.RawURLEncoding.DecodeString(raw)
	if err != nil {
		return nil, ErrCursorInvalid
	}
	var p cursorPayload
	if err := json.Unmarshal(b, &p); err != nil {
		return nil, ErrCursorInvalid
	}
	t, err := time.Parse(time.RFC3339Nano, p.T)
	if err != nil {
		return nil, ErrCursorInvalid
	}
	id, err := uuid.Parse(p.I)
	if err != nil {
		return nil, ErrCursorInvalid
	}
	return &repository.EntryCursor{CreatedAt: t, ID: id}, nil
}
