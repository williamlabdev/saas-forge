package metrics

import (
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestRegistry_Handler(t *testing.T) {
	reg := NewRegistry()
	reg.AuthLoginSuccess.Add(3)
	reg.OutboxDelivered.Add(1)
	reg.SetOutboxGauges(2, 1.5, 4, 12.0)

	rec := httptest.NewRecorder()
	reg.Handler().ServeHTTP(rec, httptest.NewRequest("GET", "/metrics", nil))
	require.Equal(t, 200, rec.Code)
	body := rec.Body.String()
	assert.Contains(t, body, "auth_login_success_total 3")
	assert.Contains(t, body, "outbox_delivered_total 1")
	assert.Contains(t, body, "outbox_pending 2")
	assert.Contains(t, body, "outbox_lag_seconds 1.500")
	assert.Contains(t, body, "outbox_processing 4")
	assert.Contains(t, body, "outbox_processing_lag_seconds 12.000")
	assert.Contains(t, body, "process_uptime_seconds")
}
