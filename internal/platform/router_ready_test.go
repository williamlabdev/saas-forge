package platform

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/require"

	"github.com/williamlabdev/saas-forge/internal/pkg/config"
)

// readyRouter builds the real router with every handler nil'd out — the same
// shape agentRouter (router_agent_ratelimit_test.go) uses — since only
// /health and /ready are exercised here and neither touches a handler.
func readyRouter(t *testing.T, pool *pgxpool.Pool, gatewaySecret string) http.Handler {
	t.Helper()
	return NewRouter(
		nil, nil, nil, nil, nil, nil, nil, nil,
		nil, false, activeChecker{}, nil,
		false, "", gatewaySecret,
		ProvideAgentRateLimiter(config.Runtime{}),
		pool,
	)
}

func TestRouter_ReadyReportsDBDown(t *testing.T) {
	// pgxpool.New does not dial — it is lazy — so this succeeds without a real
	// database. The pool then targets a port nothing listens on, so Ping fails
	// fast (connection refused) rather than hanging for readyTimeout.
	pool, err := pgxpool.New(context.Background(), "postgres://nouser:nopass@127.0.0.1:1/nodb?sslmode=disable&connect_timeout=1")
	require.NoError(t, err)
	defer pool.Close()

	h := readyRouter(t, pool, "")

	req := httptest.NewRequest(http.MethodGet, "/ready", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	require.Equal(t, http.StatusServiceUnavailable, rec.Code)
	var body map[string]string
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &body))
	require.NotEmpty(t, body["db"])
	require.NotEqual(t, "ok", body["db"])
}

func TestRouter_ReadyWithNoPoolConfiguredIsUnavailable(t *testing.T) {
	h := readyRouter(t, nil, "")

	req := httptest.NewRequest(http.MethodGet, "/ready", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	require.Equal(t, http.StatusServiceUnavailable, rec.Code)
}

// /ready must answer even when GATEWAY_SECRET is set and the caller cannot
// present it — the same exemption /health already has (router.go's
// GatewayGuard predicate). A probe that started 403-ing the moment an
// operator turned the gateway guard on would take every replica out of
// rotation at once.
func TestRouter_ReadyExemptFromGatewayGuard(t *testing.T) {
	h := readyRouter(t, nil, "s3cret")

	req := httptest.NewRequest(http.MethodGet, "/ready", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	require.NotEqual(t, http.StatusForbidden, rec.Code, "must not be blocked by the gateway guard")
}
