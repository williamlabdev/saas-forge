package mock

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestServer_UpsertUserState(t *testing.T) {
	s := NewServer()
	id := uuid.New()

	body, _ := json.Marshal(map[string]any{
		"user_id":         id.String(),
		"status":          "active",
		"status_version":  1,
		"event_type":      "user.created",
		"idempotency_key": "k1",
	})
	req := httptest.NewRequest(http.MethodPut, "/v1/users/state", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, req)
	require.Equal(t, http.StatusOK, rec.Code)
	require.Equal(t, 1, s.Count())
	assert.Equal(t, id, s.Upserts()[0].UserID)
}
